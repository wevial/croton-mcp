package mcpserver_test

import (
	"testing"

	"github.com/wevial/croton-mcp/internal/testkit"
)

// A decoded folder name containing "&" is an exact, valid destination even
// when it resembles a modified UTF-7 shift. Its wire form escapes each
// ampersand as "&-".
func TestTriageMoveExactFolderWithLiteralAmpersand(t *testing.T) {
	for _, folder := range []struct{ decoded, wire string }{
		{"Folders/Research&-Development", "Folders/Research&--Development"},
		{"Folders/R&D-2026", "Folders/R&-D-2026"},
	} {
		t.Run(folder.wire, func(t *testing.T) {
			options := triageSeeds()
			options.Mailboxes = append(options.Mailboxes, testkit.MailboxSeed{Name: folder.wire, UIDValidity: 8200})
			h := triageStart(t, options, map[string]any{"enabled": true})
			h.requireTools(t, "move_mail")
			before := h.fixture.Snapshot()

			uids := []uint32{102, 101}
			triageResults(t, h.call(t, "move_mail", triageMoveArgs(uids, folder.decoded)), uids, []string{"applied", "applied"}, "")
			triageMoved(t, before, h.fixture.Snapshot(), "INBOX", folder.wire, uids)
			triageMoveWire(t, h, uids, folder.wire)
		})
	}
}

// A malformed modified UTF-7 wire name elsewhere in the account is never
// listed by the exact destination LIST, so it cannot block a valid move.
func TestTriageMoveIgnoresUnrelatedMalformedWireName(t *testing.T) {
	options := triageSeeds()
	options.Mailboxes = append(options.Mailboxes, testkit.MailboxSeed{Name: "Folders/&A-", UIDValidity: 8100})
	h := triageStart(t, options, map[string]any{"enabled": true})
	h.requireTools(t, "move_mail")
	before := h.fixture.Snapshot()

	uids := []uint32{101}
	triageResults(t, h.call(t, "move_mail", triageMoveArgs(uids, "Folders/Existing")), uids, []string{"applied"}, "")
	triageMoved(t, before, h.fixture.Snapshot(), "INBOX", "Folders/Existing", uids)
	triageMoveWire(t, h, uids, "Folders/Existing")
}
