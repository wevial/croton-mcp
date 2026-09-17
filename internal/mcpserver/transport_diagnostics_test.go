package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestTransportDiagnostics(t *testing.T) {
	t.Parallel()

	sentinels := []string{"credential-sentinel", "person@privacy.test", "mailbox-sentinel", "body-sentinel"}
	wrap := func(err error) error {
		return fmt.Errorf("%s: %w", strings.Join(sentinels, " "), err)
	}

	cases := []struct {
		name     string
		err      error
		category string
		wantErr  error
	}{
		{"nil", nil, "normal_close", nil},
		{"wrapped_io_EOF", wrap(io.EOF), "normal_close", nil},
		{"wrapped_mcp_ErrConnectionClosed", wrap(mcp.ErrConnectionClosed), "normal_close", nil},
		{"wrapped_context_Canceled", wrap(context.Canceled), "canceled", nil},
		{"wrapped_syscall_EPIPE", wrap(syscall.EPIPE), "client_disconnected", errServerUnavailable},
		{"synthetic_unknown", wrap(errors.New("synthetic unknown")), "transport_failure", errServerUnavailable},
		{"error_text_is_not_identity", wrap(errors.New("EOF context canceled broken pipe")), "transport_failure", errServerUnavailable},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, enabled := range []bool{true, false} {
				t.Run(fmt.Sprintf("audit_enabled_%t", enabled), func(t *testing.T) {
					var diagnostics bytes.Buffer
					var auditor *Auditor
					if enabled {
						auditor = NewAuditor(&diagnostics)
					}

					var transport mcp.Transport = failingTransport{err: tc.err}
					if tc.err == nil {
						// An empty stream closes normally, so SDK Run returns nil.
						transport = &mcp.IOTransport{
							Reader: io.NopCloser(strings.NewReader("")),
							Writer: diagnosticTestWriter{Writer: io.Discard},
						}
					}

					err := Serve(context.Background(), New(Options{Audit: auditor}), transport)
					if err != tc.wantErr {
						t.Fatalf("Serve error = %v, want %v", err, tc.wantErr)
					}
					if err != nil && err.Error() != "server unavailable" {
						t.Fatalf("unexpected public error: %v", err)
					}

					for _, sentinel := range sentinels {
						if strings.Contains(diagnostics.String(), sentinel) || (err != nil && strings.Contains(err.Error(), sentinel)) {
							t.Fatalf("diagnostic or public error leaked sentinel %q", sentinel)
						}
					}

					if !enabled {
						if diagnostics.Len() != 0 {
							t.Fatalf("disabled diagnostics = %q", diagnostics.String())
						}
						return
					}

					requireTransportEnd(t, &diagnostics, tc.category)
				})
			}
		})
	}

	t.Run("separated_streams", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		clientRead, serverWrite := io.Pipe()
		serverRead, clientWrite := io.Pipe()
		defer clientRead.Close()
		defer serverRead.Close()
		defer clientWrite.Close()
		defer serverWrite.Close()

		var protocol, diagnostics bytes.Buffer
		serverTransport := &mcp.IOTransport{
			Reader: serverRead,
			Writer: diagnosticTestWriter{
				Writer: io.MultiWriter(serverWrite, &protocol),
				closer: serverWrite,
			},
		}
		done := make(chan error, 1)

		go func() {
			done <- Serve(ctx, New(Options{Audit: NewAuditor(&diagnostics)}), serverTransport)
		}()

		client := mcp.NewClient(&mcp.Implementation{Name: "diagnostic-test", Version: "1"}, nil)
		session, err := client.Connect(ctx, &mcp.IOTransport{Reader: clientRead, Writer: clientWrite}, nil)
		if err != nil {
			t.Fatalf("Connect: %v", err)
		}

		if _, err := session.ListTools(ctx, nil); err != nil {
			t.Fatalf("ListTools: %v", err)
		}
		if err := session.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Serve: %v", err)
			}
		case <-ctx.Done():
			t.Fatal("Serve did not finish")
		}

		requireTransportEnd(t, &diagnostics, "normal_close")

		lines := strings.Split(strings.TrimSpace(protocol.String()), "\n")
		if len(lines) < 2 {
			t.Fatalf("expected initialization and tools responses, got %q", protocol.String())
		}

		for _, line := range lines {
			var message map[string]json.RawMessage
			if err := json.Unmarshal([]byte(line), &message); err != nil {
				t.Fatalf("invalid protocol JSON: %v", err)
			}
			if string(message["jsonrpc"]) != `"2.0"` || message["id"] == nil || message["result"] == nil {
				t.Fatalf("invalid JSON-RPC response: %s", line)
			}
			if message["event"] != nil || strings.Contains(line, "transport_end") {
				t.Fatalf("lifecycle event in protocol stream: %s", line)
			}
		}
	})
}

func requireTransportEnd(t *testing.T, diagnostics *bytes.Buffer, category string) {
	t.Helper()

	events := auditLines(t, diagnostics)
	if len(events) != 1 {
		t.Fatalf("expected one lifecycle event, got %q", diagnostics.String())
	}

	event := events[0]
	if len(event) != 2 || event["event"] != "transport_end" || event["category"] != category {
		t.Fatalf("unexpected lifecycle schema or values: %v", event)
	}

	switch event["category"] {
	case "normal_close", "canceled", "client_disconnected", "transport_failure":
	default:
		t.Fatalf("non-allowlisted lifecycle category: %v", event)
	}
}

type diagnosticTestWriter struct {
	io.Writer
	closer io.Closer
}

func (writer diagnosticTestWriter) Close() error {
	if writer.closer != nil {
		return writer.closer.Close()
	}

	return nil
}
