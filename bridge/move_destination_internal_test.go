package bridge

import "testing"

func TestValidateMoveDestinationAcceptsExactUTF8Names(t *testing.T) {
	for _, test := range []struct {
		destination string
		code        string
	}{
		{"Folders/Existing", ""},
		{"Folders/Research&-Development", ""},
		{"Folders/R&D-2026", ""},
		{"Folders/R&D", ""},
		{"Folders/A & B", ""},
		{"Folders/Trailing&", ""},
		{"Folders/&A-", ""},
		{"Folders/Reçus", ""},
		{"", CodeInvalidRequest},
		{"Folders/Existing\x00", CodeInvalidRequest},
		{"Folders/Existing\r\nA9 EXPUNGE", CodeInvalidRequest},
		{"Folders/*", CodeInvalidRequest},
		{"Folders/%", CodeInvalidRequest},
		{"Folders/\xff", CodeInvalidRequest},
	} {
		if code := CodeOf(validateMoveDestination(test.destination)); code != test.code {
			t.Errorf("validateMoveDestination(%q) = %q, want %q", test.destination, code, test.code)
		}
	}
}

func TestResolveMoveDestinationAcceptsExactFolderWithLiteralAmpersand(t *testing.T) {
	listing := []listedMailbox{
		{Name: "INBOX", Delimiter: '/'},
		{Name: "Folders", Delimiter: '/', Attributes: []string{`\Noselect`}},
		{Name: "Folders/Research&-Development", Delimiter: '/'},
		{Name: "Folders/R&D-2026", Delimiter: '/'},
		{Name: "Folders/Research&Development", Delimiter: '/', Attributes: []string{`\Noselect`}},
	}

	for _, name := range []string{"Folders/Research&-Development", "Folders/R&D-2026"} {
		target, ok := resolveMoveDestination(listing, name)
		if !ok || target != name {
			t.Errorf("resolveMoveDestination(%q) = %q, %t; want the exact folder", name, target, ok)
		}
	}
	if _, ok := resolveMoveDestination(listing, "Folders/Research&Development"); ok {
		t.Error("resolveMoveDestination accepted a nonselectable near-match")
	}
}
