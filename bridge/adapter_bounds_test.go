package bridge_test

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/testkit"
)

func TestAdapterRejectsOversizedBodyLiteralsDespitePartialFetch(t *testing.T) {
	const maxBodyBytes = 32

	for _, test := range []struct {
		name     string
		scenario testkit.Scenario
	}{
		{
			name:     "server ignores partial range",
			scenario: testkit.Scenario{IgnoreBodyPartial: true},
		},
		{
			name:     "literal declaration is max plus one",
			scenario: testkit.Scenario{OversizedBodyLiteralBytes: maxBodyBytes + 1},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			server, err := testkit.Start(testkit.Options{Mode: testkit.ImplicitTLS, Scenario: test.scenario})
			if err != nil {
				t.Fatalf("start fake server: %v", err)
			}
			t.Cleanup(func() { _ = server.Close() })

			config := fakeServerConfig(t, server.Addr(), bridge.TLSModeImplicit, bridge.TLSConfig{SPKISHA256: server.SPKISHA256()})
			config.Bounds.MaxBodyBytes = bridge.Int(maxBodyBytes)
			adapter, err := bridge.NewAdapter(config)
			if err != nil {
				t.Fatalf("NewAdapter: %v", err)
			}
			t.Cleanup(func() { _ = adapter.Close() })

			results, err := adapter.SearchMail(context.Background(), bridge.SearchQuery{Mailbox: "INBOX"})
			if err != nil || len(results) != 1 {
				t.Fatalf("SearchMail = %#v, %v", results, err)
			}
			if _, err := adapter.GetMessageBody(context.Background(), results[0].ID); bridge.CodeOf(err) != bridge.CodeBoundsExceeded {
				t.Fatalf("GetMessageBody error = %v, want %q", err, bridge.CodeBoundsExceeded)
			}

			assertTranscriptContains(t, server.Commands(), "EXAMINE INBOX", "UID SEARCH UID 3:102", "UID FETCH 101", "BODY.PEEK[HEADER]", "BODY.PEEK[]<0.33>")
			if err := server.AssertReadOnlyCommands(); err != nil {
				t.Fatalf("read-only transcript: %v", err)
			}
		})
	}
}

func TestAdapterBoundedLiteralAbortReleasesFetchReader(t *testing.T) {
	before := runtime.NumGoroutine()

	for attempt := 0; attempt < 8; attempt++ {
		server, err := testkit.Start(testkit.Options{
			Mode:     testkit.ImplicitTLS,
			Scenario: testkit.Scenario{OversizedBodyLiteralBytes: 33},
		})
		if err != nil {
			t.Fatalf("start fake server: %v", err)
		}

		config := fakeServerConfig(t, server.Addr(), bridge.TLSModeImplicit, bridge.TLSConfig{SPKISHA256: server.SPKISHA256()})
		config.Bounds.MaxBodyBytes = bridge.Int(32)
		adapter, err := bridge.NewAdapter(config)
		if err != nil {
			_ = server.Close()
			t.Fatalf("NewAdapter: %v", err)
		}

		results, err := adapter.SearchMail(context.Background(), bridge.SearchQuery{Mailbox: "INBOX"})
		if err != nil || len(results) != 1 {
			_ = adapter.Close()
			_ = server.Close()
			t.Fatalf("SearchMail = %#v, %v", results, err)
		}
		if _, err := adapter.GetMessageBody(context.Background(), results[0].ID); bridge.CodeOf(err) != bridge.CodeBoundsExceeded {
			_ = adapter.Close()
			_ = server.Close()
			t.Fatalf("GetMessageBody error = %v, want %q", err, bridge.CodeBoundsExceeded)
		}
		if err := adapter.Close(); err != nil {
			_ = server.Close()
			t.Fatalf("Close: %v", err)
		}
		if err := server.Close(); err != nil {
			t.Fatalf("close fake server: %v", err)
		}
	}

	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > before+3 && time.Now().Before(deadline) {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > before+3 {
		t.Fatalf("goroutines after bounded literal abort = %d, want at most %d", got, before+3)
	}
}

func assertTranscriptContains(t *testing.T, commands []testkit.Command, fragments ...string) {
	t.Helper()

	transcript := make([]string, 0, len(commands))
	for _, command := range commands {
		transcript = append(transcript, command.Raw)
	}
	joined := strings.Join(transcript, "\n")
	for _, fragment := range fragments {
		if !strings.Contains(joined, fragment) {
			t.Fatalf("transcript %q does not contain %q", joined, fragment)
		}
	}
}
