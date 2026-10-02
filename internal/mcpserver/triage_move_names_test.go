package mcpserver_test

import (
	"testing"

	"github.com/wevial/croton-mcp/internal/testkit"
)

// A decoded folder name containing a literal "&-" is an exact, valid
// destination. Its modified UTF-7 wire form escapes the ampersand as "&-".
func TestTriageMoveExactFolderWithLiteralAmpersand(t *testing.T) {
	const decoded = "Folders/Research&-Development"
	const wire = "Folders/Research&--Development"

	options := triageSeeds()
	options.Mailboxes = append(options.Mailboxes, testkit.MailboxSeed{Name: wire, UIDValidity: 8200})
	h := triageStart(t, options, map[string]any{"enabled": true})
	h.requireTools(t, "move_mail")
	before := h.fixture.Snapshot()

	uids := []uint32{102, 101}
	triageResults(t, h.call(t, "move_mail", triageMoveArgs(uids, decoded)), uids, []string{"applied", "applied"}, "")
	triageMoved(t, before, h.fixture.Snapshot(), "INBOX", wire, uids)
	triageMoveWire(t, h, uids, wire)
}
