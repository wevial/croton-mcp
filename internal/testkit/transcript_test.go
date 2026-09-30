package testkit

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestStatefulTranscript(t *testing.T) {
	t.Parallel()

	t.Run("exact_operands", func(t *testing.T) {
		t.Parallel()

		valid := []struct {
			raw  string
			want TranscriptCommand
		}{
			{`a1 UID STORE 7 +FLAGS (\Seen)`, TranscriptCommand{
				Tag: "a1", Verb: "UID STORE", Arguments: `7 +FLAGS (\Seen)`,
				Set: "7", Ranges: []SetRange{{7, 7}}, Store: "+FLAGS", Flags: []string{`\Seen`},
			}},
			{`a2 UID STORE 1,3,5 -FLAGS.SILENT (\Seen)`, TranscriptCommand{
				Tag: "a2", Verb: "UID STORE", Arguments: `1,3,5 -FLAGS.SILENT (\Seen)`,
				Set: "1,3,5", Ranges: []SetRange{{1, 1}, {3, 3}, {5, 5}}, Store: "-FLAGS", Silent: true, Flags: []string{`\Seen`},
			}},
			{`a3 uid store 4:2 +flags.silent \Seen`, TranscriptCommand{
				Tag: "a3", Verb: "UID STORE", Arguments: `4:2 +flags.silent \Seen`,
				Set: "4:2", Ranges: []SetRange{{4, 2}}, Store: "+FLAGS", Silent: true, Flags: []string{`\Seen`},
			}},
			{`a4 UID STORE 9:*,4294967295 -FLAGS \Seen $Junk`, TranscriptCommand{
				Tag: "a4", Verb: "UID STORE", Arguments: `9:*,4294967295 -FLAGS \Seen $Junk`,
				Set: "9:*,4294967295", Ranges: []SetRange{{9, 0}, {4294967295, 4294967295}}, Store: "-FLAGS", Flags: []string{`\Seen`, `$Junk`},
			}},
			{`a5 STORE 2 FLAGS ()`, TranscriptCommand{
				Tag: "a5", Verb: "STORE", Arguments: `2 FLAGS ()`,
				Set: "2", Ranges: []SetRange{{2, 2}}, Store: "FLAGS", Flags: []string{},
			}},
			{`a6 UID MOVE 2,5,9 "Folders/Old Archive"`, TranscriptCommand{
				Tag: "a6", Verb: "UID MOVE", Arguments: `2,5,9 "Folders/Old Archive"`,
				Set: "2,5,9", Ranges: []SetRange{{2, 2}, {5, 5}, {9, 9}}, Destination: "Folders/Old Archive",
			}},
			{`a7 UID MOVE 1:3 "Say \"UID STORE 1 +FLAGS\" \\ now"`, TranscriptCommand{
				Tag: "a7", Verb: "UID MOVE", Arguments: `1:3 "Say \"UID STORE 1 +FLAGS\" \\ now"`,
				Set: "1:3", Ranges: []SetRange{{1, 3}}, Destination: `Say "UID STORE 1 +FLAGS" \ now`,
			}},
			{`a8 UID MOVE 4 Archive]`, TranscriptCommand{
				Tag: "a8", Verb: "UID MOVE", Arguments: `4 Archive]`,
				Set: "4", Ranges: []SetRange{{4, 4}}, Destination: "Archive]",
			}},
			{`a9 UID COPY 1 "UID MOVE 1 Trash"`, TranscriptCommand{
				Tag: "a9", Verb: "UID COPY", Arguments: `1 "UID MOVE 1 Trash"`,
				Set: "1", Ranges: []SetRange{{1, 1}}, Destination: "UID MOVE 1 Trash",
			}},
			{`b1 COPY 3 MOVE`, TranscriptCommand{
				Tag: "b1", Verb: "COPY", Arguments: "3 MOVE",
				Set: "3", Ranges: []SetRange{{3, 3}}, Destination: "MOVE",
			}},
			{`b2 move 3 ""`, TranscriptCommand{
				Tag: "b2", Verb: "MOVE", Arguments: `3 ""`,
				Set: "3", Ranges: []SetRange{{3, 3}}, Destination: "",
			}},
			{`b3 SELECT "UID MOVE 1 Trash"`, TranscriptCommand{Tag: "b3", Verb: "SELECT", Arguments: `"UID MOVE 1 Trash"`}},
			{`b4 NOOP`, TranscriptCommand{Tag: "b4", Verb: "NOOP"}},
			{`b5 XMOVE 1 Trash`, TranscriptCommand{Tag: "b5", Verb: "XMOVE", Arguments: "1 Trash"}},
			{`b6 UID FETCH 1:* (UID FLAGS)`, TranscriptCommand{Tag: "b6", Verb: "UID FETCH", Arguments: "1:* (UID FLAGS)"}},
		}

		commands := make([]Command, 0, len(valid))
		want := make([]TranscriptCommand, 0, len(valid))
		for index, testCase := range valid {
			command := Command{Sequence: index + 1, ConnectionID: 1 + index%3, Raw: testCase.raw, TLS: index%2 == 0}
			testCase.want.Sequence, testCase.want.ConnectionID, testCase.want.TLS = command.Sequence, command.ConnectionID, command.TLS
			commands = append(commands, command)
			want = append(want, testCase.want)

			got, err := ParseTranscriptCommand(command)
			if err != nil || !reflect.DeepEqual(got, testCase.want) {
				t.Errorf("parse %q = %+v, %v\nwant %+v", testCase.raw, got, err, testCase.want)
			}
		}

		got, err := ParseTranscript(commands)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("ParseTranscript = %+v, %v\nwant %+v", got, err, want)
		}

		malformed := []string{
			``,
			`a1`,
			`a1 `,
			` a1 UID MOVE 1 Trash`,
			`a1  UID MOVE 1 Trash`,
			`* UID MOVE 1 Trash`,
			`+ UID MOVE 1 Trash`,
			`a1 UID`,
			`a1 UID `,
			`a1 UID  MOVE 1 Trash`,
			`a1 NOOP `,
			`a1 UID MOVE 1`,
			`a1 UID MOVE 1 `,
			`a1 UID MOVE 1  Trash`,
			`a1 UID MOVE 1 Trash extra`,
			`a1 UID MOVE 1 Trash)`,
			`a1 UID MOVE 1 (Trash)`,
			`a1 UID MOVE 1 "Trash`,
			`a1 UID MOVE 1 "Tr\ash"`,
			`a1 UID MOVE 1 "Trash" extra`,
			`a1 UID MOVE 1 {5}`,
			`a1 UID MOVE 1 {5+}`,
			"a1 UID MOVE 1 Tr\tash",
			"a1 UID MOVE 1 \"Trash\r\"",
			`a1 UID MOVE 0 Trash`,
			`a1 UID MOVE 01 Trash`,
			`a1 UID MOVE 4294967296 Trash`,
			`a1 UID MOVE 1: Trash`,
			`a1 UID MOVE :1 Trash`,
			`a1 UID MOVE 1:2:3 Trash`,
			`a1 UID MOVE 1,,2 Trash`,
			`a1 UID MOVE 1, Trash`,
			`a1 UID MOVE ** Trash`,
			`a1 UID MOVE 1a Trash`,
			`a1 UID MOVE Trash`,
			`a1 UID COPY 1 "MOVE" MOVE`,
			`a1 UID STORE 1 +FLAGS`,
			`a1 UID STORE 1 +FLAGS `,
			`a1 UID STORE 1 +FLAGS (\Seen`,
			`a1 UID STORE 1 +FLAGS (\Seen) extra`,
			`a1 UID STORE 1 +FLAGS (\Seen))`,
			`a1 UID STORE 1 +FLAGS ((\Seen))`,
			`a1 UID STORE 1 +FLAGS ( \Seen)`,
			`a1 UID STORE 1 +FLAGS (\Seen )`,
			`a1 UID STORE 1 +FLAGS (\Seen  \Flagged)`,
			`a1 UID STORE 1 +FLAGS (\)`,
			`a1 UID STORE 1 +FLAGS (\\Seen)`,
			`a1 UID STORE 1 +FLAGS ("\Seen")`,
			`a1 UID STORE 1 +FLAGS \Seen)`,
			`a1 UID STORE 1 +FLAGS(\Seen)`,
			`a1 UID STORE 1 *FLAGS (\Seen)`,
			`a1 UID STORE 1 +FLAGS.LOUD (\Seen)`,
			`a1 UID STORE 1 FLAGS.SILENT.SILENT (\Seen)`,
			`a1 UID STORE 1 +flag (\Seen)`,
			`a1 UID STORE +FLAGS (\Seen)`,
			`a1 UID STORE 1,2 3 +FLAGS (\Seen)`,
		}
		for _, raw := range malformed {
			command := Command{Sequence: 7, ConnectionID: 2, Raw: raw, TLS: true}
			if got, err := ParseTranscriptCommand(command); !errors.Is(err, errMalformedTranscript) || !reflect.DeepEqual(got, TranscriptCommand{}) {
				t.Errorf("parse %q = %+v, %v; want malformed", raw, got, err)
			}
		}

		secret := Command{Sequence: 3, Raw: "a1 LOGIN fixture-user@client.test not-a-secret"}
		_, err = ParseTranscript([]Command{secret, {Sequence: 4, Raw: "a2 UID MOVE 1 Trash extra"}})
		if !errors.Is(err, errMalformedTranscript) || !strings.Contains(err.Error(), "sequence 4") || strings.Contains(err.Error(), "not-a-secret") {
			t.Fatalf("ParseTranscript error = %v", err)
		}
	})
}
