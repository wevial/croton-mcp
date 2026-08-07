package bridge

import "context"

// Folder is one bounded mailbox result. Name originates from the configured
// local Bridge and is returned only after a successful LIST completion.
type Folder struct {
	Name      string
	Delimiter string
}

// MailboxStatus contains safe, read-only mailbox counters.
type MailboxStatus struct {
	Messages    int
	UIDNext     uint32
	UIDValidity uint32
	Unseen      int
}

// ListFolders returns at most the configured folder limit. The adapter never
// exposes IMAP LIST options or a caller-controlled wire pattern.
func (adapter *Adapter) ListFolders(ctx context.Context) ([]Folder, error) {
	var folders []Folder
	err := adapter.execute(ctx, func(operationContext context.Context, session readSession) error {
		result, err := session.List(operationContext, adapter.config.Bounds.MaxFolderResults)
		if err != nil {
			return err
		}
		if len(result) > adapter.config.Bounds.MaxFolderResults {
			return errorCode(CodeBoundsExceeded)
		}

		folders = result
		return nil
	})
	if err != nil {
		return nil, err
	}

	return folders, nil
}

// Status returns selected counters without changing the selected mailbox.
func (adapter *Adapter) Status(ctx context.Context, mailbox string) (MailboxStatus, error) {
	if mailbox == "" {
		return MailboxStatus{}, errorCode(CodeMailboxNotFound)
	}

	var status MailboxStatus
	err := adapter.execute(ctx, func(operationContext context.Context, session readSession) error {
		result, err := session.Status(operationContext, mailbox)
		if err != nil {
			return err
		}

		status = result
		return nil
	})
	if err != nil {
		return MailboxStatus{}, err
	}

	return status, nil
}
