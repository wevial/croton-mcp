package bridge

import "testing"

func TestValidateMoveDestinationRejectsOnlyMalformedNames(t *testing.T) {
	for _, test := range []struct {
		destination string
		code        string
	}{
		{"Folders/Existing", ""},
		{"Folders/Research&-Development", ""},
		{"Folders/R&D", ""},
		{"Folders/A & B", ""},
		{"Folders/Trailing&", ""},
		{"Folders/Reçus", ""},
		{"Folders/&A-", CodeInvalidRequest},
		{"Folders/Re&AOc-us", CodeInvalidRequest},
		{"Folders/Research&-&A-", CodeInvalidRequest},
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
		{Name: "Folders/Research&Development", Delimiter: '/', Attributes: []string{`\Noselect`}},
	}

	target, ok := resolveMoveDestination(listing, "Folders/Research&-Development")
	if !ok || target != "Folders/Research&-Development" {
		t.Fatalf("resolveMoveDestination = %q, %t; want the exact folder", target, ok)
	}
	if _, ok := resolveMoveDestination(listing, "Folders/Research&Development"); ok {
		t.Fatal("resolveMoveDestination accepted a nonselectable near-match")
	}
}
