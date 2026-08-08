package mcpserver

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestBoundedFrameReaderPassesBoundedNDJSON(t *testing.T) {
	t.Parallel()

	input := "{\"jsonrpc\":\"2.0\",\"id\":1}\n{\"jsonrpc\":\"2.0\",\"id\":2}\n"
	got, err := io.ReadAll(newBoundedFrameReader(strings.NewReader(input), maxStdioFrameBytes))
	if err != nil {
		t.Fatalf("read bounded frames: %v", err)
	}
	if string(got) != input {
		t.Fatalf("frames changed: %q", got)
	}
}

func TestBoundedFrameReaderRejectsOversizeFrameBeforeDelivery(t *testing.T) {
	t.Parallel()

	input := strings.Repeat("S", maxStdioFrameBytes) + "\n"
	encoded, err := io.ReadAll(newBoundedFrameReader(strings.NewReader(input), maxStdioFrameBytes))
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
		`{"jsonrpc":"2.0","id":1,"method":"resources/list","Method":"ping"}` + "\n",
		`{"jsonrpc":"2.0","id":1,"method":"ping","params":{},"paramſ":{"ignored":true}}` + "\n",
	}
	for _, frame := range frames {
		encoded, err := io.ReadAll(newBoundedFrameReader(strings.NewReader(frame), maxStdioFrameBytes))
		if err == nil {
			t.Fatalf("ambiguous protocol frame was accepted: %s", frame)
		}
		if len(encoded) != 0 {
			t.Fatalf("ambiguous frame bytes delivered = %d, want 0", len(encoded))
		}
	}
}

func TestBoundedFrameReaderAcceptsOfficialOpenCapabilityMapKeys(t *testing.T) {
	t.Parallel()

	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"experimental":{"VendorFeature":{},"vendorfeature":{}}},"clientInfo":{"name":"fixture-client","version":"1"}}}` + "\n")
	var envelope struct {
		Params mcp.InitializeParams `json:"params"`
	}
	if err := json.Unmarshal(frame, &envelope); err != nil {
		t.Fatalf("official SDK type rejected fixture: %v", err)
	}
	if envelope.Params.Capabilities == nil || len(envelope.Params.Capabilities.Experimental) != 2 {
		t.Fatalf("official SDK lost distinct open-map keys: %+v", envelope.Params.Capabilities)
	}

	encoded, err := io.ReadAll(newBoundedFrameReader(strings.NewReader(string(frame)), maxStdioFrameBytes))
	if err != nil {
		t.Fatalf("bounded stdio rejected official-SDK-valid frame: %v", err)
	}
	if string(encoded) != string(frame) {
		t.Fatalf("frame changed during validation: %q", encoded)
	}
}
