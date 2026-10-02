package bridge

import "context"

// MaxTriageUIDs bounds one triage request.
const MaxTriageUIDs = 50

// Per-UID triage outcomes, reported in input order.
const (
	OutcomeApplied      = "applied"
	OutcomeRefused      = "refused"
	OutcomeNotAttempted = "not_attempted"
	OutcomeUnknown      = "unknown"
)

// TriageResult is the outcome for one requested UID. Code is a stable bridge
// error code and is empty when the UID was applied or never reached.
type TriageResult struct {
	UID     uint32
	Outcome string
	Code    string
}

// SetSeen adds or removes only the \Seen flag on each UID of one source
// mailbox generation. It refreshes UIDVALIDITY on the session that will write
// and fails the whole request before any write when it differs. UIDs are then
// processed one at a time in input order: a UID no longer present is refused
// without dispatch, and an uncertain completion stops all further writes and
// is never replayed.
func (adapter *Adapter) SetSeen(ctx context.Context, mailbox string, uidValidity uint32, uids []uint32, seen bool) ([]TriageResult, error) {
	if err := validateTriageRequest(mailbox, uidValidity, uids); err != nil {
		return nil, err
	}

	if err := adapter.acquire(ctx); err != nil {
		return nil, err
	}
	defer func() { adapter.gate <- struct{}{} }()

	session, writer, err := adapter.openTriageSource(ctx, mailbox, uidValidity)
	if err != nil {
		return nil, err
	}

	results := make([]TriageResult, len(uids))
	for index, uid := range uids {
		results[index] = TriageResult{UID: uid, Outcome: OutcomeNotAttempted}
	}

	for index, uid := range uids {
		result, stop := adapter.storeSeen(ctx, session, writer, uid, seen)
		results[index] = result
		if stop {
			break
		}
	}

	return results, nil
}

func validateTriageRequest(mailbox string, uidValidity uint32, uids []uint32) error {
	if mailbox == "" || uidValidity == 0 || len(uids) == 0 {
		return errorCode(CodeInvalidRequest)
	}
	if len(mailbox) > maxMailboxNameBytes || len(uids) > MaxTriageUIDs {
		return errorCode(CodeBoundsExceeded)
	}

	seen := make(map[uint32]struct{}, len(uids))
	for _, uid := range uids {
		if uid == 0 {
			return errorCode(CodeInvalidRequest)
		}
		if _, duplicate := seen[uid]; duplicate {
			return errorCode(CodeInvalidRequest)
		}
		seen[uid] = struct{}{}
	}

	return nil
}

// openTriageSource selects the source read-write and checks its generation.
// Nothing has been written yet, so one reconnect of a dead retained session
// is as safe here as on the read path.
func (adapter *Adapter) openTriageSource(ctx context.Context, mailbox string, uidValidity uint32) (readSession, seenSession, error) {
	for attempt := 0; attempt < 2; attempt++ {
		operationContext, cancel := adapter.operationContext(ctx)
		session, writer, snapshot, err := adapter.selectTriageSource(operationContext, mailbox)
		if err != nil {
			err = mapIMAPError(operationContext, err)
			retry := attempt == 0 && adapter.canReplay(operationContext, err)
			cancel()

			if session != nil && (retry || adapter.invalidatesSession(err)) {
				adapter.invalidate(session)
			}
			if retry {
				continue
			}

			return nil, nil, err
		}
		cancel()

		if snapshot.UIDValidity != uidValidity {
			return nil, nil, errorCode(CodeStaleMessageID)
		}

		return session, writer, nil
	}

	return nil, nil, errorCode(CodeBridgeUnreachable)
}

func (adapter *Adapter) selectTriageSource(ctx context.Context, mailbox string) (readSession, seenSession, mailboxSnapshot, error) {
	session, err := adapter.sessionForOperation(ctx)
	if err != nil {
		return nil, nil, mailboxSnapshot{}, err
	}

	writer, ok := session.(seenSession)
	if !ok {
		return session, nil, mailboxSnapshot{}, errorCode(CodeIMAPProtocol)
	}

	snapshot, err := writer.SelectSource(ctx, mailbox)
	return session, writer, snapshot, err
}

// storeSeen confirms one UID is present, then dispatches its single STORE.
// Every step has its own command deadline, so the batch latency is bounded.
// It reports whether every later UID must be left unattempted.
func (adapter *Adapter) storeSeen(ctx context.Context, session readSession, writer seenSession, uid uint32, seen bool) (TriageResult, bool) {
	operationContext, cancel := adapter.operationContext(ctx)
	defer cancel()

	if err := requireMessageUID(operationContext, session, uid); err != nil {
		if CodeOf(err) == CodeStaleMessageID {
			return TriageResult{UID: uid, Outcome: OutcomeRefused, Code: CodeMessageNotFound}, false
		}

		// Nothing was dispatched for this UID, but the session is no longer
		// trustworthy, so no later UID is attempted either.
		adapter.invalidate(session)
		return TriageResult{UID: uid, Outcome: OutcomeNotAttempted, Code: CodeOf(mapIMAPError(operationContext, err))}, true
	}

	err := writer.StoreSeen(operationContext, uid, seen)
	if err == nil {
		return TriageResult{UID: uid, Outcome: OutcomeApplied}, false
	}

	err = mapIMAPError(operationContext, err)
	if CodeOf(err) == CodeIMAPCommand {
		// A tagged NO or BAD is a definite refusal of this UID only.
		return TriageResult{UID: uid, Outcome: OutcomeRefused, Code: CodeIMAPCommand}, false
	}

	// The STORE may or may not have applied. Drop the session so the
	// in-flight command cannot complete later, and never replay it.
	adapter.invalidate(session)
	return TriageResult{UID: uid, Outcome: OutcomeUnknown, Code: CodeOf(err)}, true
}
