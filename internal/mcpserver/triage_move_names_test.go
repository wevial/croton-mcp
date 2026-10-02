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

// Destinations are resolved from a full ordinary LIST, which decodes every
// wire name. A malformed modified UTF-7 name anywhere in the account fails
// that listing before any write, so the whole request fails closed.
func TestTriageMoveFailsClosedOnMalformedWireName(t *testing.T) {
	options := triageSeeds()
	options.Mailboxes = append(options.Mailboxes, testkit.MailboxSeed{Name: "Folders/&A-", UIDValidity: 8100})
	h := triageStart(t, options, map[string]any{"enabled": true})
	h.requireTools(t, "move_mail")
	before := h.fixture.Snapshot()

	result := h.call(t, "move_mail", triageMoveArgs([]uint32{101}, "Folders/Existing"))
	if result == nil || !result.IsError {
		t.Fatal("move after an undecodable listing did not fail the request")
	}
	triageNoWrites(t, h, before)
}
