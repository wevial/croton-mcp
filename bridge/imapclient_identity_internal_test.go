package bridge

import (
	"fmt"
	"testing"
)

func TestValidateFetchedMetadataUIDsRequiresExactRequestedSet(t *testing.T) {
	tests := []struct {
		name      string
		requested []uint32
		results   []MessageMetadata
		wantCode  string
	}{
		{
			name:      "exact set",
			requested: []uint32{101, 102},
			results:   []MessageMetadata{{uid: 102}, {uid: 101}},
		},
		{
			name:      "substituted UID",
			requested: []uint32{101, 102},
			results:   []MessageMetadata{{uid: 101}, {uid: 103}},
			wantCode:  CodeIMAPProtocol,
		},
		{
			name:      "duplicate UID",
			requested: []uint32{101, 102},
			results:   []MessageMetadata{{uid: 101}, {uid: 101}},
			wantCode:  CodeIMAPProtocol,
		},
		{
			name:      "missing UID",
			requested: []uint32{101, 102},
			results:   []MessageMetadata{{uid: 101}},
			wantCode:  CodeIMAPProtocol,
		},
		{
			name:      "duplicate requested UID",
			requested: []uint32{101, 101},
			results:   []MessageMetadata{{uid: 101}, {uid: 101}},
			wantCode:  CodeIMAPProtocol,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateFetchedMetadataUIDs(test.requested, test.results)
			if CodeOf(err) != test.wantCode {
				t.Fatalf("validateFetchedMetadataUIDs() error = %v, want %q", err, test.wantCode)
			}
		})
	}
}

func TestSearchObserverSkipsLiteralPayloadAcrossEveryChunkSplit(t *testing.T) {
	for _, test := range []struct {
		name    string
		literal string
	}{
		{name: "SEARCH", literal: "* SEARCH\r\n"},
		{name: "ESEARCH", literal: "* ESEARCH\r\n"},
	} {
		for _, marker := range []struct {
			name   string
			format string
		}{
			{name: "literal", format: "{%d}"},
			{name: "nonsynchronizing-literal", format: "{%d+}"},
			{name: "literal8", format: "~{%d}"},
		} {
			input := fmt.Sprintf("* 1 FETCH (BODY[] "+marker.format+"\r\n%s)\r\n", len(test.literal), test.literal)
			for split := 0; split <= len(input); split++ {
				t.Run(fmt.Sprintf("%s/%s/split-%d", test.name, marker.name, split), func(t *testing.T) {
					connection := &readBudgetConn{}
					connection.observeSearchResponseLocked(input[:split])
					connection.observeSearchResponseLocked(input[split:])
					if connection.searchResponseSeen || connection.eSearchResponseSeen {
						t.Fatalf("literal payload was treated as SEARCH proof: search=%v esearch=%v", connection.searchResponseSeen, connection.eSearchResponseSeen)
					}
					connection.observeSearchResponseLocked("* SEARCH\r\n")
					if !connection.searchResponseSeen {
						t.Fatal("real SEARCH response after literal was not observed")
					}
				})
			}
		}
	}
}

func TestSearchObserverDetectsDuplicateUIDsBeforeSetNormalization(t *testing.T) {
	for _, test := range []struct {
		name  string
		input string
		want  bool
	}{
		{name: "distinct SEARCH", input: "* SEARCH 7 8\r\n", want: false},
		{name: "repeated SEARCH UID", input: "* SEARCH 7 7\r\n", want: true},
		{name: "repeated across SEARCH lines", input: "* SEARCH 7\r\n* SEARCH 7\r\n", want: true},
		{name: "SEARCH MODSEQ group", input: "* SEARCH 7 (MODSEQ 7)\r\n", want: false},
		{name: "repeated SEARCH UID before unspaced MODSEQ group", input: "* SEARCH 7 7(MODSEQ 5)\r\n", want: true},
		{name: "distinct ESEARCH ALL", input: "* ESEARCH (TAG \"T1\") UID ALL 1:3,4:6\r\n", want: false},
		{name: "wide ESEARCH range", input: "* ESEARCH (TAG \"T1\") UID ALL 1:100000000\r\n", want: false},
		{name: "repeated ESEARCH ALL UID", input: "* ESEARCH (TAG \"T1\") UID ALL 7,7\r\n", want: true},
		{name: "overlapping reversed ESEARCH range", input: "* ESEARCH (TAG \"T1\") UID ALL 5:3,4\r\n", want: true},
		{name: "ESEARCH COUNT only", input: "* ESEARCH (TAG \"T1\") UID COUNT 7\r\n", want: false},
		{name: "distinct ESEARCH ALL after literal TAG", input: "* ESEARCH (TAG {2}\r\nT1) UID ALL 7,8\r\n", want: false},
		{name: "repeated ESEARCH ALL UID after literal TAG", input: "* ESEARCH (TAG {2}\r\nT1) UID ALL 7,7\r\n", want: true},
		{name: "repeated ESEARCH ALL UID after literal with parenthesis", input: "* ESEARCH (TAG {3}\r\nT)1) UID ALL 7,7\r\n", want: true},
		{name: "repeated ESEARCH ALL UID after quoted parenthesis", input: "* ESEARCH (TAG \"T)1\") UID ALL 7,7\r\n", want: true},
		{name: "repeated ESEARCH ALL UID after nested extension ALL", input: "* ESEARCH (TAG \"T1\") UID X (F ALL 7) ALL 7,7\r\n", want: true},
		{name: "repeated ESEARCH ALL UID after nested extension literal", input: "* ESEARCH (TAG \"T1\") UID X ({3}\r\nA)L) ALL 7,7\r\n", want: true},
		{name: "repeated ESEARCH ALL UID without UID marker", input: "* ESEARCH (TAG \"T1\") ALL 7,7\r\n", want: true},
		{name: "repeated UID only inside nested extension ALL", input: "* ESEARCH (TAG \"T1\") UID X (F ALL 7,7) ALL 7\r\n", want: false},
		{name: "repeated ESEARCH ALL UID in lowercase field", input: "* ESEARCH (TAG \"T1\") UID COUNT 2 all 7:8,8\r\n", want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			connection := &readBudgetConn{}
			connection.beginSearch(maxSearchResponseBytes)

			connection.observeSearchResponseLocked(test.input)

			if observed := connection.endSearch(); observed.duplicate != test.want {
				t.Fatalf("duplicate = %v, want %v", observed.duplicate, test.want)
			}
		})
	}
}

func TestSearchObserverFailsClosedOnLinesItCannotWalk(t *testing.T) {
	for _, test := range []struct {
		name  string
		input string
		want  bool
	}{
		{name: "ESEARCH unspaced extension value", input: "* ESEARCH (TAG \"T1\") UID X(F ALL 101) ALL 101,101\r\n", want: true},
		{name: "ESEARCH unclosed correlator", input: "* ESEARCH (TAG \"T1\" UID ALL 7\r\n", want: true},
		{name: "ESEARCH unparseable ALL set", input: "* ESEARCH (TAG \"T1\") UID ALL 7:x\r\n", want: true},
		{name: "SEARCH unspaced MODSEQ group", input: "* SEARCH 7 8(MODSEQ 5)\r\n", want: false},
		{name: "SEARCH group other than MODSEQ", input: "* SEARCH 7 (X 5)\r\n", want: true},
		{name: "SEARCH numbers after MODSEQ group", input: "* SEARCH 7 (MODSEQ 5) 7\r\n", want: true},
		{name: "SEARCH numbers", input: "* SEARCH 7 8\r\n", want: false},
		{name: "SEARCH MODSEQ group", input: "* SEARCH 7 (MODSEQ 7)\r\n", want: false},
		{name: "empty SEARCH", input: "* SEARCH\r\n", want: false},
		{name: "empty ESEARCH", input: "* ESEARCH (TAG \"T1\") UID\r\n", want: false},
		{name: "ESEARCH spaced extension value", input: "* ESEARCH (TAG \"T1\") UID X (F ALL 7) ALL 7\r\n", want: false},
		{name: "ESEARCH COUNT and ALL", input: "* ESEARCH (TAG \"T1\") UID COUNT 2 ALL 7:8\r\n", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			connection := &readBudgetConn{}
			connection.beginSearch(maxSearchResponseBytes)

			connection.observeSearchResponseLocked(test.input)

			if observed := connection.endSearch(); observed.incomplete != test.want {
				t.Fatalf("incomplete = %v, want %v", observed.incomplete, test.want)
			}
		})
	}
}
