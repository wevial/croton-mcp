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
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestOrdinaryToolInvalidParamsKeepsSessionOpen(t *testing.T) {
	local, remote := net.Pipe()
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	session, err := server.Connect(t.Context(), NewTransport(local, local), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	defer remote.Close()
	if err := remote.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}

	const meta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`
	decoder := json.NewDecoder(remote)
	type response struct {
		ID     int
		Error  *jsonrpc.Error
		Result json.RawMessage
	}
	call := func(frame string) response {
		t.Helper()
		if _, err := io.WriteString(remote, frame+"\n"); err != nil {
			t.Fatal(err)
		}
		var reply response
		if err := decoder.Decode(&reply); err != nil {
			t.Fatal(err)
		}
		return reply
	}

	bad := call(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_folders","inputResponses":false,` + meta + `}}`)
	if bad.ID != 1 || bad.Error == nil || bad.Error.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("expected invalid-params response, got %+v", bad)
	}
	list := call(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{` + meta + `}}`)
	if list.ID != 2 || list.Error != nil || len(list.Result) == 0 {
		t.Fatalf("expected tool list on same session, got %+v", list)
	}
}

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
