package bridge

import (
	"context"
	"net"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/wevial/croton-mcp/internal/testkit"
)

func startTLSHeaderAdapter(t *testing.T, mode testkit.TLSMode, options testkit.Options) (*testkit.Server, *Adapter) {
	t.Helper()

	options.Mode = mode
	server, err := testkit.Start(options)
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	host, port, err := net.SplitHostPort(server.Addr())
	if err != nil {
		t.Fatalf("split fake server address: %v", err)
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("parse fake server port: %v", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}

	tlsMode := TLSModeStartTLS
	if mode == testkit.ImplicitTLS {
		tlsMode = TLSModeImplicit
	}
	adapter, err := NewAdapter(Config{IMAP: IMAPConfig{
		Host:              host,
		Port:              number,
		TLSMode:           tlsMode,
		CredentialCommand: []string{executable, "-test.run=TestCredentialHelperProcess", "--", "valid"},
		TLS:               TLSConfig{SPKISHA256: server.SPKISHA256()},
		ConnectTimeoutMs:  1000,
	}})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close() })

	return server, adapter
}

func requireStructureRefusal(t *testing.T, name string, result MessageAttachmentStructure, err error, code string) {
	t.Helper()

	if CodeOf(err) != code {
		t.Fatalf("%s error = %v, want %s", name, err, code)
	}
	if !reflect.DeepEqual(result, MessageAttachmentStructure{}) {
		t.Fatalf("%s returned a normal result %+v", name, result)
	}
}

// fetchesSince counts FETCH commands recorded after the first from commands.
func fetchesSince(server *testkit.Server, from int) int {
	fetches := 0
	for _, command := range server.Commands()[from:] {
		if strings.Contains(strings.ToUpper(command.Raw), " FETCH ") {
			fetches++
		}
	}

	return fetches
}

// attachmentDisposition is an attachment disposition with params.
func attachmentDisposition(params ...testkit.BodyParam) *testkit.BodyDisposition {
	return &testkit.BodyDisposition{Value: "attachment", Params: params}
}

// plainDescriptor is a single application/octet-stream part with an
// optional attachment disposition and its parameters.
func plainDescriptor(size uint32, params []testkit.BodyParam, disposition *testkit.BodyDisposition) *testkit.BodyPart {
	return &testkit.BodyPart{Kind: testkit.BodyPartBasic, Type: "application", Subtype: "octet-stream", Encoding: "base64", Size: size, Params: params, Disposition: disposition}
}

func TestAttachmentStructureAdapter(t *testing.T) {
	var _ func(*Adapter, context.Context, string) (MessageAttachmentStructure, error) = (*Adapter).GetMessageAttachmentStructure
	if fields := reflect.TypeFor[MessageAttachmentStructure]().NumField(); fields != 6 {
		t.Fatalf("MessageAttachmentStructure has %d fields, want 6", fields)
	}

	root := t.TempDir()
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "TMPDIR"} {
		t.Setenv(name, root)
	}

	t.Run("identity_and_generation", func(t *testing.T) {
		attachment := plainDescriptor(4096, nil, attachmentDisposition(testkit.BodyParam{Name: "filename", Value: "report.pdf"}, testkit.BodyParam{Name: "size", Value: "3000"}))
		server, adapter := startTLSHeaderAdapter(t, testkit.StartTLS, testkit.Options{
			Messages:       []string{"Subject: one\r\n\r\nfirst\r\n", "Subject: two\r\n\r\nsecond\r\n"},
			BodyStructures: []*testkit.BodyPart{{Kind: testkit.BodyPartText, Type: "text", Subtype: "plain", Size: 7, Lines: 1}, attachment},
		})
		ctx := context.Background()
		opened := 0
		adapter.factory = func(ctx context.Context) (bridgeSession, error) {
			opened++
			return adapter.openSession(ctx)
		}

		// The current ID returns exactly its own tuple and the projection.
		identifier := headerTestID(t, adapter, 9001, 102)
		result, err := adapter.GetMessageAttachmentStructure(ctx, identifier)
		if err != nil {
			t.Fatalf("current ID error = %v", err)
		}
		want := MessageAttachmentStructure{
			ID: identifier, Mailbox: "INBOX", UIDValidity: 9001, UID: 102,
			Attachments: []AttachmentStructureEntry{{
				AttachmentMetadata: AttachmentMetadata{Filename: "report.pdf", ContentType: "application/octet-stream", Disposition: "attachment", DeclaredSize: 3000, HasDeclaredSize: true},
			}},
		}
		if !reflect.DeepEqual(result, want) {
			t.Fatalf("current ID result = %+v, want %+v", result, want)
		}
		if fetches := fetchesSince(server, 0); fetches != 1 {
			t.Fatalf("current ID issued %d FETCH commands", fetches)
		}

		// A stale generation is refused after a fresh selection and
		// before any FETCH; the existence SEARCH may run.
		before := len(server.Commands())
		result, err = adapter.GetMessageAttachmentStructure(ctx, headerTestID(t, adapter, 9002, 102))
		requireStructureRefusal(t, "stale generation", result, err, CodeStaleMessageID)
		if fetches := fetchesSince(server, before); fetches != 0 {
			t.Fatalf("stale generation issued %d FETCH commands", fetches)
		}
		examined := false
		for _, command := range server.Commands()[before:] {
			examined = examined || command.Name == "EXAMINE"
		}
		if !examined {
			t.Fatal("stale generation was not decided on a fresh selection")
		}

		// A missing UID in the current generation is stale before any FETCH.
		before = len(server.Commands())
		result, err = adapter.GetMessageAttachmentStructure(ctx, headerTestID(t, adapter, 9001, 150))
		requireStructureRefusal(t, "missing UID", result, err, CodeStaleMessageID)
		if fetches := fetchesSince(server, before); fetches != 0 {
			t.Fatalf("missing UID issued %d FETCH commands", fetches)
		}

		// An ID from a previous process key never reaches the server.
		restarted := &Adapter{}
		restarted.idKey[0] = 0x7e
		before = len(server.Commands())
		result, err = adapter.GetMessageAttachmentStructure(ctx, headerTestID(t, restarted, 9001, 102))
		requireStructureRefusal(t, "restart-invalid ID", result, err, CodeStaleMessageID)
		if got := len(server.Commands()); got != before {
			t.Fatalf("restart-invalid ID issued %d commands", got-before)
		}

		// Every call ran read-only on the one pooled session: one
		// connection, one credential helper run and one LOGIN.
		if err := server.AssertReadOnlyCommands(); err != nil {
			t.Fatalf("read-only transcript: %v", err)
		}
		connections := map[int]bool{}
		logins := 0
		for _, command := range server.Commands() {
			connections[command.ConnectionID] = true
			if command.Name == "LOGIN" {
				logins++
			}
		}
		if opened != 1 || len(connections) != 1 || logins != 1 {
			t.Fatalf("calls opened %d sessions over %d connections with %d LOGINs, want one each", opened, len(connections), logins)
		}
	})

	t.Run("plain_metadata_and_size", func(t *testing.T) {
		filename := testkit.BodyParam{Name: "filename", Value: "file.bin"}
		cases := []struct {
			name       string
			descriptor *testkit.BodyPart
			want       []AttachmentStructureEntry
		}{
			{
				name:       "filename_before_name_absent_size",
				descriptor: plainDescriptor(512, []testkit.BodyParam{{Name: "name", Value: "type-name.bin"}}, &testkit.BodyDisposition{Value: "INLINE", Params: []testkit.BodyParam{filename}}),
				want:       []AttachmentStructureEntry{attachmentEntry("file.bin", "application/octet-stream", "inline")},
			},
			{
				name:       "content_type_name_without_disposition",
				descriptor: plainDescriptor(77, []testkit.BodyParam{{Name: "name", Value: "type-name.bin"}}, nil),
				want:       []AttachmentStructureEntry{attachmentEntry("type-name.bin", "application/octet-stream", "")},
			},
			{
				name:       "attachment_disposition_without_name_zero_size",
				descriptor: plainDescriptor(0, nil, attachmentDisposition(testkit.BodyParam{Name: "size", Value: "0"})),
				want:       []AttachmentStructureEntry{{AttachmentMetadata: AttachmentMetadata{ContentType: "application/octet-stream", Disposition: "attachment", HasDeclaredSize: true}}},
			},
			{
				name:       "positive_size",
				descriptor: plainDescriptor(4000, nil, attachmentDisposition(filename, testkit.BodyParam{Name: "size", Value: "2900"})),
				want:       []AttachmentStructureEntry{{AttachmentMetadata: AttachmentMetadata{Filename: "file.bin", ContentType: "application/octet-stream", Disposition: "attachment", DeclaredSize: 2900, HasDeclaredSize: true}}},
			},
			{
				name:       "negative_size",
				descriptor: plainDescriptor(9, nil, attachmentDisposition(filename, testkit.BodyParam{Name: "size", Value: "-1"})),
				want:       []AttachmentStructureEntry{attachmentEntry("file.bin", "application/octet-stream", "attachment")},
			},
			{
				name:       "invalid_size",
				descriptor: plainDescriptor(9, nil, attachmentDisposition(filename, testkit.BodyParam{Name: "size", Value: "12kb"})),
				want:       []AttachmentStructureEntry{attachmentEntry("file.bin", "application/octet-stream", "attachment")},
			},
			{
				// ParseInt saturates to MaxInt64 with ErrRange; the size
				// stays unknown rather than taking the saturated value.
				name:       "int64_overflow_size",
				descriptor: plainDescriptor(9, nil, attachmentDisposition(filename, testkit.BodyParam{Name: "size", Value: "9223372036854775808"})),
				want:       []AttachmentStructureEntry{attachmentEntry("file.bin", "application/octet-stream", "attachment")},
			},
			{
				name:       "int64_maximum_size",
				descriptor: plainDescriptor(4294967295, nil, attachmentDisposition(filename, testkit.BodyParam{Name: "size", Value: "9223372036854775807"})),
				want:       []AttachmentStructureEntry{{AttachmentMetadata: AttachmentMetadata{Filename: "file.bin", ContentType: "application/octet-stream", Disposition: "attachment", DeclaredSize: 9223372036854775807, HasDeclaredSize: true}}},
			},
			{
				name:       "text_leaf_with_name",
				descriptor: &testkit.BodyPart{Kind: testkit.BodyPartText, Type: "TEXT", Subtype: "CSV", Params: []testkit.BodyParam{{Name: "NAME", Value: "rows.csv"}}, Size: 31, Lines: 3},
				want:       []AttachmentStructureEntry{attachmentEntry("rows.csv", "text/csv", "")},
			},
			{
				name:       "plain_nonattachment_leaf",
				descriptor: &testkit.BodyPart{Kind: testkit.BodyPartText, Type: "text", Subtype: "plain", Size: 5, Lines: 1, Disposition: &testkit.BodyDisposition{Value: "inline"}},
				want:       []AttachmentStructureEntry{},
			},
		}

		messages := make([]string, 0, len(cases))
		descriptors := make([]*testkit.BodyPart, 0, len(cases))
		for index := range cases {
			// The raw bytes are unrelated to every descriptor, so no size
			// can come from the message itself.
			messages = append(messages, "Subject: plain "+cases[index].name+"\r\n\r\n"+strings.Repeat("r", 100+index)+"\r\n")
			descriptors = append(descriptors, cases[index].descriptor)
		}
		server, adapter := startTLSHeaderAdapter(t, testkit.ImplicitTLS, testkit.Options{Messages: messages, BodyStructures: descriptors})

		for index, testCase := range cases {
			uid := uint32(101 + index)
			result, err := adapter.GetMessageAttachmentStructure(context.Background(), headerTestID(t, adapter, 9001, uid))
			if err != nil {
				t.Fatalf("%s error = %v", testCase.name, err)
			}
			if result.UID != uid || result.Truncation.Any() || result.Attachments == nil {
				t.Fatalf("%s result = %+v", testCase.name, result)
			}
			if !reflect.DeepEqual(result.Attachments, testCase.want) {
				t.Fatalf("%s attachments = %+v, want %+v", testCase.name, result.Attachments, testCase.want)
			}
		}
		if err := server.AssertReadOnlyCommands(); err != nil {
			t.Fatalf("read-only transcript: %v", err)
		}
	})
}
