// Copyright 2026 Ko
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package stdioframe

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestBoundedFrameReaderPassesBoundedNDJSON(t *testing.T) {
	t.Parallel()

	input := "{\"jsonrpc\":\"2.0\",\"id\":1}\n{\"jsonrpc\":\"2.0\",\"id\":2}\n"
	got, err := io.ReadAll(newBoundedFrameReader(strings.NewReader(input), MaxFrameBytes))
	if err != nil {
		t.Fatalf("read bounded frames: %v", err)
	}
	if string(got) != input {
		t.Fatalf("frames changed: %q", got)
	}
}

func TestBoundedFrameReaderRejectsOversizeFrameBeforeDelivery(t *testing.T) {
	t.Parallel()

	input := strings.Repeat("S", MaxFrameBytes) + "\n"
	encoded, err := io.ReadAll(newBoundedFrameReader(strings.NewReader(input), MaxFrameBytes))
	if !errors.Is(err, errStdioFrameTooLarge) {
		t.Fatalf("error = %v, want %v", err, errStdioFrameTooLarge)
	}
	if len(encoded) != 0 {
		t.Fatalf("oversize frame bytes delivered = %d, want 0", len(encoded))
	}
}

func TestBoundedFrameReaderRejectsAmbiguousJSONBeforeDelivery(t *testing.T) {
	t.Parallel()

	frames := []string{
		`{"jsonrpc":"2.0","id":1,"method":"ping"} {"secret":"ignored"}` + "\n",
		`{"jsonrpc":"2.0","id":1,"method":"resources/list","method":"ping"}` + "\n",
	}
	for _, frame := range frames {
		encoded, err := io.ReadAll(newBoundedFrameReader(strings.NewReader(frame), MaxFrameBytes))
		if err == nil {
			t.Fatalf("ambiguous protocol frame was accepted: %s", frame)
		}
		if len(encoded) != 0 {
			t.Fatalf("ambiguous frame bytes delivered = %d, want 0", len(encoded))
		}
	}
}

func TestForwardFrameRequestID(t *testing.T) {
	const uuid = `"edb503e2-8f0b-4b7b-8a13-9fb2eeb3ab53"`
	longID := `"` + strings.Repeat("x", 37) + `"`
	for _, tc := range []struct {
		id, tool string
		refuse   bool
	}{
		{"1", "send_forward", false},
		{uuid, "send_forward", false},
		{strings.Replace(uuid, "5", `\u0035`, 1), "send_forward", true},
		{longID, "send_forward", true},
		{longID, "prepare_forward", false},
	} {
		frame := `{"jsonrpc":"2.0","method":"tools/call","id":` + tc.id + `,"ID":1,"params":{"name":"` + tc.tool + `"}}` + "\n"
		got, err := io.ReadAll(newBoundedFrameReader(strings.NewReader(frame), MaxFrameBytes))
		if tc.refuse {
			if !errors.Is(err, errStdioFrameInvalid) || len(got) != 0 {
				t.Fatal("oversized forward ID reached the SDK")
			}
		} else if err != nil || string(got) != frame {
			t.Fatal("valid frame rejected", err)
		}
	}
}
