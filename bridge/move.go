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
// existing destination. Self-moves are refused for every UID before any
// connection is used; the shared batch rules are those of move.
func (adapter *Adapter) Move(ctx context.Context, mailbox string, uidValidity uint32, uids []uint32, destination string) ([]TriageResult, error) {
	if err := validateTriageRequest(mailbox, uidValidity, uids); err != nil {
		return nil, err
	}
	if err := validateMoveDestination(destination); err != nil {
		return nil, err
	}

	if mailboxIdentityMatches(mailbox, destination) {
		results := pendingResults(uids)
		refuseRemaining(results)
		return results, nil
	}

	return adapter.move(ctx, mailbox, uidValidity, uids, func(listing []listedMailbox) (string, bool) {
		return resolveMoveDestination(listing, destination)
	})
}

// Archive natively moves each UID to the one mailbox the fresh listing marks
// with the \Archive attribute. See moveToAttribute.
func (adapter *Adapter) Archive(ctx context.Context, mailbox string, uidValidity uint32, uids []uint32) ([]TriageResult, error) {
	return adapter.moveToAttribute(ctx, mailbox, uidValidity, uids, `\Archive`)
}

// Trash natively moves each UID to the one mailbox the fresh listing marks
// with the \Trash attribute. It never deletes, expunges or empties anything.
// See moveToAttribute.
func (adapter *Adapter) Trash(ctx context.Context, mailbox string, uidValidity uint32, uids []uint32) ([]TriageResult, error) {
	return adapter.moveToAttribute(ctx, mailbox, uidValidity, uids, `\Trash`)
}

// Resolve the attribute from each fresh listing before checking for a self-move.
func (adapter *Adapter) moveToAttribute(ctx context.Context, mailbox string, uidValidity uint32, uids []uint32, attribute string) ([]TriageResult, error) {
	if err := validateTriageRequest(mailbox, uidValidity, uids); err != nil {
		return nil, err
	}

	return adapter.move(ctx, mailbox, uidValidity, uids, func(listing []listedMailbox) (string, bool) {
		target, ok := resolveAttributeDestination(listing, attribute)
		if !ok || mailboxIdentityMatches(mailbox, target) {
			return "", false
		}

		return target, true
	})
}

// destinationResolver picks the exact move target from one fresh listing, or
// reports that the destination is unsupported.
type destinationResolver func([]listedMailbox) (string, bool)

// Confirm each UID and resolve its destination on the writing session. An
// unsupported destination refuses the rest of the batch. An uncertain MOVE
// stops the batch without replay.
func (adapter *Adapter) move(ctx context.Context, mailbox string, uidValidity uint32, uids []uint32, resolve destinationResolver) ([]TriageResult, error) {
	results := pendingResults(uids)

	if err := adapter.acquire(ctx); err != nil {
		return nil, err
	}
	defer func() { adapter.gate <- struct{}{} }()

	session, err := adapter.openTriageSource(ctx, mailbox, uidValidity)
	if err != nil {
		return nil, err
	}

	if !session.SupportsMove() {
		refuseRemaining(results)
		return results, nil
	}

	written := false
	for index, uid := range uids {
		result, stop := adapter.moveUID(ctx, session, uid, resolve)
		results[index] = result
		if result.Code == CodeUnsupported {
			refuseRemaining(results[index+1:])
			break
		}
		if stop && !written && result.Outcome == OutcomeNotAttempted {
			return nil, errorCode(result.Code)
		}
		if stop {
			break
		}
		written = written || result.Outcome == OutcomeApplied
	}

	return results, nil
}

func pendingResults(uids []uint32) []TriageResult {
	results := make([]TriageResult, len(uids))
	for index, uid := range uids {
		results[index] = TriageResult{UID: uid, Outcome: OutcomeNotAttempted}
	}

	return results
}

// Validate the decoded mailbox name. The LIST parser checks wire encoding.
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

func refuseRemaining(results []TriageResult) {
	for index := range results {
		results[index] = TriageResult{UID: results[index].UID, Outcome: OutcomeRefused, Code: CodeUnsupported}
	}
}

// moveUID dispatches one UID MOVE after prepareMove validated it. It reports
// whether every later UID must be left unattempted.
func (adapter *Adapter) moveUID(ctx context.Context, session bridgeSession, uid uint32, resolve destinationResolver) (TriageResult, bool) {
	target, refusal, stop := adapter.prepareMove(ctx, session, uid, resolve)
	if target == "" {
		return refusal, stop
	}

	operationContext, cancel := adapter.operationContext(ctx)
	defer cancel()

	err := session.MoveUID(operationContext, uid, target)
	if err == nil {
		return TriageResult{UID: uid, Outcome: OutcomeApplied}, false
	}

	err = mapIMAPError(operationContext, err)
	switch CodeOf(err) {
	case CodeUnsupported:
		// MOVE stopped being advertised and nothing was sent: the check
		// refused first, or the dispatch guard refused a fallback write and
		// closed the connection, so the session is discarded.
		adapter.invalidate(session)
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

// Recheck membership and the destination on the dispatch session before each MOVE.
func (adapter *Adapter) prepareMove(ctx context.Context, session bridgeSession, uid uint32, resolve destinationResolver) (string, TriageResult, bool) {
	operationContext, cancel := adapter.operationContext(ctx)
	defer cancel()

	if err := requireMessageUID(operationContext, session, uid); err != nil {
		if CodeOf(err) == CodeStaleMessageID {
			return "", TriageResult{UID: uid, Outcome: OutcomeRefused, Code: CodeMessageNotFound}, false
		}

		return "", adapter.abandonUID(operationContext, session, uid, err), true
	}

	listing, err := session.ListMailboxes(operationContext, "*", adapter.config.Bounds.MaxFolderResults)
	if err != nil {
		return "", adapter.abandonUID(operationContext, session, uid, err), true
	}

	target, ok := resolve(listing)
	if !ok {
		return "", TriageResult{UID: uid, Outcome: OutcomeRefused, Code: CodeUnsupported}, false
	}

	return target, TriageResult{}, false
}

// abandonUID reports a UID for which nothing was dispatched on a session that
// is no longer trustworthy, so no later UID is attempted either.
func (adapter *Adapter) abandonUID(ctx context.Context, session bridgeSession, uid uint32, err error) TriageResult {
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

// resolveAttributeDestination maps attribute to the one listed mailbox that
// carries it. Several holders, selectable or not, are ambiguous. The holder
// must then pass every explicit destination rule, so Labels/, virtual views
// and nonselectable mailboxes are refused, and it must not be a Bridge
// aggregate view even when that view's \All or \Flagged attribute is missing.
// A name only ever narrows the mapping; it never authorizes one.
func resolveAttributeDestination(listing []listedMailbox, attribute string) (string, bool) {
	var holder *listedMailbox
	for index := range listing {
		if !hasAttribute(listing[index].Attributes, []string{attribute}) {
			continue
		}
		if holder != nil {
			return "", false
		}
		holder = &listing[index]
	}
	if holder == nil || strings.EqualFold(holder.Name, "All Mail") || strings.EqualFold(holder.Name, "Starred") {
		return "", false
	}

	return resolveMoveDestination(listing, holder.Name)
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
