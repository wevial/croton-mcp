package bridge

import "testing"

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
