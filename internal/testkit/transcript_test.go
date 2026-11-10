package testkit

import "testing"

func TestParseTranscriptCommand(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		raw, verb, set string
	}{
		{`a1 uid store 7 +FLAGS.SILENT (\Seen)`, "UID STORE", "7"},
		{`a2 UID MOVE 1:3 "Folders/Old Archive"`, "UID MOVE", "1:3"},
		{`a3 SELECT "UID MOVE 1 Trash"`, "SELECT", ""},
		{`a4 NOOP`, "NOOP", ""},
		{`a5`, "", ""},
	} {
		got := ParseTranscriptCommand(Command{Raw: test.raw})
		if got.Verb != test.verb || got.Set != test.set {
			t.Errorf("parse %q = %q %q, want %q %q", test.raw, got.Verb, got.Set, test.verb, test.set)
		}
	}
}
