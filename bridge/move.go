package bridge

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"
)

// foldersPrefix is the only user-folder namespace accepted as a destination.
const foldersPrefix = "Folders/"

// rootSystemAttributes are the protocol attributes that authorize a
// hierarchy-root destination. An English name alone never does.
var rootSystemAttributes = []string{`\Sent`, `\Drafts`, `\Junk`, `\Archive`, `\Trash`}

// refusedDestinationAttributes mark nonselectable mailboxes and virtual views.
var refusedDestinationAttributes = []string{`\Noselect`, `\NonExistent`, `\All`, `\Flagged`}

// Move natively moves each UID of one source mailbox generation to one exact,
// existing destination. Self-moves and servers without MOVE are refused for
// every UID before any write. Otherwise it refreshes UIDVALIDITY on the
// session that will write and fails the whole request before any write when
// it differs. UIDs are then processed one at a time in input order: each is
// confirmed present and the destination is revalidated from a fresh ordinary
// LIST on the same session immediately before its single UID MOVE. A missing
// UID or a definitive NO refuses that UID only, an unsupported destination
// refuses every remaining UID, and an uncertain completion stops all further
// writes and is never replayed. There is no COPY or EXPUNGE fallback.
func (adapter *Adapter) Move(ctx context.Context, mailbox string, uidValidity uint32, uids []uint32, destination string) ([]TriageResult, error) {
	if err := validateTriageRequest(mailbox, uidValidity, uids); err != nil {
		return nil, err
	}
	if err := validateMoveDestination(destination); err != nil {
		return nil, err
	}

	results := make([]TriageResult, len(uids))
	for index, uid := range uids {
		results[index] = TriageResult{UID: uid, Outcome: OutcomeNotAttempted}
	}

	if mailboxIdentityMatches(mailbox, destination) {
		refuseRemaining(results, 0)
		return results, nil
	}

	if err := adapter.acquire(ctx); err != nil {
		return nil, err
	}
	defer func() { adapter.gate <- struct{}{} }()

	session, _, err := adapter.openTriageSource(ctx, mailbox, uidValidity)
	if err != nil {
		return nil, err
	}

	mover, ok := session.(moveSession)
	if !ok {
		return nil, errorCode(CodeIMAPProtocol)
	}
	if !mover.SupportsMove() {
		refuseRemaining(results, 0)
		return results, nil
	}

	for index, uid := range uids {
		result, stop := adapter.moveUID(ctx, session, mover, uid, destination)
		results[index] = result
		if result.Code == CodeUnsupported {
			refuseRemaining(results, index+1)
			break
		}
		if stop {
			break
		}
	}

	return results, nil
}

// validateMoveDestination rejects destinations that are not one exact,
// printable UTF-8 mailbox name before any connection is used. Wildcards are
// refused so the destination can serve as an exact LIST pattern. Malformed
// modified UTF-7 is a property of wire names, not of decoded input, so it is
// handled by the exact LIST in prepareMove rather than guessed here.
func validateMoveDestination(destination string) error {
	if destination == "" || !utf8.ValidString(destination) {
		return errorCode(CodeInvalidRequest)
	}
	if len(destination) > maxMailboxNameBytes {
		return errorCode(CodeBoundsExceeded)
	}

	for _, character := range destination {
		if unicode.IsControl(character) || character == '*' || character == '%' {
			return errorCode(CodeInvalidRequest)
		}
	}

	return nil
}

func refuseRemaining(results []TriageResult, from int) {
	for index := from; index < len(results); index++ {
		results[index] = TriageResult{UID: results[index].UID, Outcome: OutcomeRefused, Code: CodeUnsupported}
	}
}

// moveUID dispatches one UID MOVE after prepareMove validated it. It reports
// whether every later UID must be left unattempted.
func (adapter *Adapter) moveUID(ctx context.Context, session readSession, mover moveSession, uid uint32, destination string) (TriageResult, bool) {
	target, refusal, stop := adapter.prepareMove(ctx, session, mover, uid, destination)
	if target == "" {
		return refusal, stop
	}

	operationContext, cancel := adapter.operationContext(ctx)
	defer cancel()

	err := mover.MoveUID(operationContext, uid, target)
	if err == nil {
		return TriageResult{UID: uid, Outcome: OutcomeApplied}, false
	}

	err = mapIMAPError(operationContext, err)
	switch CodeOf(err) {
	case CodeUnsupported:
		// MOVE stopped being advertised; nothing was dispatched.
		return TriageResult{UID: uid, Outcome: OutcomeRefused, Code: CodeUnsupported}, false
	case CodeIMAPCommand:
		// A tagged NO or BAD is a definite refusal of this UID only, for
		// example when the destination disappeared after validation.
		return TriageResult{UID: uid, Outcome: OutcomeRefused, Code: CodeIMAPCommand}, false
	}

	// The MOVE may or may not have applied. Drop the session so the in-flight
	// command cannot complete later, and never replay it.
	adapter.invalidate(session)
	return TriageResult{UID: uid, Outcome: OutcomeUnknown, Code: CodeOf(err)}, true
}

// prepareMove confirms one UID is present, then resolves the destination from
// a fresh ordinary LIST on the dispatch session whose pattern is the exact,
// wildcard-free destination. The client encodes it as modified UTF-7, so only
// that one well-formed wire name can be listed; a malformed wire name
// elsewhere in the account is never listed, decoded or matched. It returns
// the exact target, or an empty target with the UID's result and whether to
// stop the batch.
func (adapter *Adapter) prepareMove(ctx context.Context, session readSession, mover moveSession, uid uint32, destination string) (string, TriageResult, bool) {
	operationContext, cancel := adapter.operationContext(ctx)
	defer cancel()

	if err := requireMessageUID(operationContext, session, uid); err != nil {
		if CodeOf(err) == CodeStaleMessageID {
			return "", TriageResult{UID: uid, Outcome: OutcomeRefused, Code: CodeMessageNotFound}, false
		}

		return "", adapter.abandonUID(operationContext, session, uid, err), true
	}

	listing, err := mover.ListMailboxes(operationContext, destination, adapter.config.Bounds.MaxFolderResults)
	if err != nil {
		return "", adapter.abandonUID(operationContext, session, uid, err), true
	}

	target, ok := resolveMoveDestination(listing, destination)
	if !ok {
		return "", TriageResult{UID: uid, Outcome: OutcomeRefused, Code: CodeUnsupported}, false
	}

	return target, TriageResult{}, false
}

// abandonUID reports a UID for which nothing was dispatched on a session that
// is no longer trustworthy, so no later UID is attempted either.
func (adapter *Adapter) abandonUID(ctx context.Context, session readSession, uid uint32, err error) TriageResult {
	adapter.invalidate(session)
	return TriageResult{UID: uid, Outcome: OutcomeNotAttempted, Code: CodeOf(mapIMAPError(ctx, err))}
}

// resolveMoveDestination accepts exactly one listed, selectable, non-virtual
// mailbox that is INBOX, an exact Folders/ entry or a hierarchy root carrying
// a system protocol attribute. Everything else, including Labels/, is refused.
func resolveMoveDestination(listing []listedMailbox, destination string) (string, bool) {
	var match *listedMailbox
	for index := range listing {
		if !mailboxIdentityMatches(destination, listing[index].Name) {
			continue
		}
		if match != nil {
			return "", false
		}
		match = &listing[index]
	}
	if match == nil || hasAttribute(match.Attributes, refusedDestinationAttributes) {
		return "", false
	}

	switch {
	case strings.EqualFold(match.Name, "INBOX"):
		return "INBOX", true
	case match.Delimiter == '/' && strings.HasPrefix(match.Name, foldersPrefix) && len(match.Name) > len(foldersPrefix):
		return match.Name, true
	case isHierarchyRoot(*match) && hasAttribute(match.Attributes, rootSystemAttributes):
		return match.Name, true
	default:
		return "", false
	}
}

func isHierarchyRoot(mailbox listedMailbox) bool {
	if strings.Contains(mailbox.Name, "/") {
		return false
	}

	return mailbox.Delimiter == 0 || !strings.ContainsRune(mailbox.Name, mailbox.Delimiter)
}

func hasAttribute(attributes, wanted []string) bool {
	for _, attribute := range attributes {
		for _, candidate := range wanted {
			if strings.EqualFold(attribute, candidate) {
				return true
			}
		}
	}

	return false
}
