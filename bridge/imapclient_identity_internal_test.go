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
