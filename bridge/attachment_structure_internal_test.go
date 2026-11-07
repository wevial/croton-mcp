package bridge

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	imap "github.com/emersion/go-imap/v2"

	"github.com/wevial/croton-mcp/internal/testkit"
)

// attachmentStructureResult mirrors MessageAttachmentStructure so the tests
// reach the method only through reflection and compile against a baseline
// without it.
type attachmentStructureResult struct {
	ID          string
	Mailbox     string
	UIDValidity uint32
	UID         uint32
	Attachments []attachmentStructureView
	Truncation  Truncation
}

type attachmentStructureView struct {
	Metadata          AttachmentMetadata
	HasTransferOctets bool
	TransferOctets    uint32
}

// callAttachmentStructure invokes Adapter.GetMessageAttachmentStructure
// through reflection, requires its result to hold exactly six fields and
// copies it into the mirror.
func callAttachmentStructure(t *testing.T, ctx context.Context, adapter *Adapter, identifier string) (attachmentStructureResult, error) {
	t.Helper()

	method := reflect.ValueOf(adapter).MethodByName("GetMessageAttachmentStructure")
	if !method.IsValid() {
		t.Fatal("Adapter.GetMessageAttachmentStructure is absent")
	}

	methodType := method.Type()
	if methodType.NumIn() != 2 || methodType.In(0) != reflect.TypeFor[context.Context]() || methodType.In(1).Kind() != reflect.String || methodType.NumOut() != 2 || methodType.Out(1) != reflect.TypeFor[error]() {
		t.Fatalf("Adapter.GetMessageAttachmentStructure has signature %s", methodType)
	}

	resultType := methodType.Out(0)
	if resultType.Name() != "MessageAttachmentStructure" || resultType.Kind() != reflect.Struct || resultType.NumField() != 6 {
		t.Fatalf("GetMessageAttachmentStructure result type %s does not hold exactly six fields", resultType)
	}

	outputs := method.Call([]reflect.Value{reflect.ValueOf(ctx), reflect.ValueOf(identifier)})
	value := outputs[0]
	result := attachmentStructureResult{}
	target := reflect.ValueOf(&result).Elem()
	for _, name := range []string{"ID", "Mailbox", "UIDValidity", "UID", "Truncation"} {
		member := value.FieldByName(name)
		if !member.IsValid() || member.Type() != target.FieldByName(name).Type() {
			t.Fatalf("MessageAttachmentStructure.%s is missing or has the wrong type", name)
		}
		target.FieldByName(name).Set(member)
	}

	attachments := value.FieldByName("Attachments")
	if !attachments.IsValid() || attachments.Kind() != reflect.Slice {
		t.Fatal("MessageAttachmentStructure.Attachments is missing or not a list")
	}
	if !attachments.IsNil() {
		result.Attachments = []attachmentStructureView{}
	}
	for index := range attachments.Len() {
		entry := attachments.Index(index)
		view := attachmentStructureView{}
		view.Metadata.Filename = entry.FieldByName("Filename").String()
		view.Metadata.ContentType = entry.FieldByName("ContentType").String()
		view.Metadata.Disposition = entry.FieldByName("Disposition").String()
		view.Metadata.DeclaredSize = entry.FieldByName("DeclaredSize").Int()
		view.Metadata.HasDeclaredSize = entry.FieldByName("HasDeclaredSize").Bool()
		view.HasTransferOctets = entry.FieldByName("HasTransferOctets").Bool()
		view.TransferOctets = uint32(entry.FieldByName("TransferOctets").Uint())
		result.Attachments = append(result.Attachments, view)
	}

	err, _ := outputs[1].Interface().(error)
	return result, err
}

// scriptedAttempt is one observed dedicated read: its context and the
// selection it was asked for.
type scriptedAttempt struct {
	ctx         context.Context
	mailbox     string
	uid         uint32
	uidValidity uint32
}

// attachmentStructureHooks reach the private dedicated-reader seam. They are
// registered by attachment_structure_seams_test.go, so this file and the TLS
// test compile against a baseline without the seam. scripted answers each
// attempt from a script and returns the observed attempts; transport runs the
// real dedicated read with wrap applied to each attempt's transport.
var attachmentStructureHooks struct {
	scripted  func(adapter *Adapter, answer func(attempt int) (selectedStructure, error)) func() []scriptedAttempt
	transport func(adapter *Adapter, wrap func(attempt int, transport net.Conn) net.Conn)
}

// wrapStructureTransport installs wrap through the transport hook.
func wrapStructureTransport(t *testing.T, adapter *Adapter, wrap func(attempt int, transport net.Conn) net.Conn) {
	t.Helper()

	if attachmentStructureHooks.transport == nil {
		t.Fatal("the dedicated-reader transport seam is absent")
	}
	attachmentStructureHooks.transport(adapter, wrap)
}

// requireStructureRefusal requires code and no normal result.
func requireStructureRefusal(t *testing.T, name string, result attachmentStructureResult, err error, code string) {
	t.Helper()

	if CodeOf(err) != code {
		t.Fatalf("%s error = %v, want %s", name, err, code)
	}
	if !reflect.DeepEqual(result, attachmentStructureResult{}) {
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

// pooledProbe is a pooled session that must never be used, replaced or
// aborted by the dedicated operation.
type pooledProbe struct {
	mu    sync.Mutex
	calls int
}

func (probe *pooledProbe) touch() {
	probe.mu.Lock()
	defer probe.mu.Unlock()

	probe.calls++
}

func (probe *pooledProbe) Authenticate(context.Context, Credentials) error {
	probe.touch()
	return nil
}
func (probe *pooledProbe) List(context.Context, int) ([]Folder, error) {
	probe.touch()
	return nil, nil
}
func (probe *pooledProbe) Status(context.Context, string) (MailboxStatus, error) {
	probe.touch()
	return MailboxStatus{}, nil
}
func (probe *pooledProbe) Examine(context.Context, string) (mailboxSnapshot, error) {
	probe.touch()
	return mailboxSnapshot{}, nil
}
func (probe *pooledProbe) UIDSearchWindow(context.Context, SearchQuery, uidWindow, int) ([]uint32, error) {
	probe.touch()
	return nil, nil
}
func (probe *pooledProbe) UIDFetchMetadata(context.Context, string, []uint32, int) ([]MessageMetadata, error) {
	probe.touch()
	return nil, nil
}
func (probe *pooledProbe) UIDFetchBody(context.Context, uint32, int) ([]byte, error) {
	probe.touch()
	return nil, nil
}
func (probe *pooledProbe) Logout(context.Context) error {
	probe.touch()
	return nil
}
func (probe *pooledProbe) Abort() error {
	probe.touch()
	return nil
}

func (probe *pooledProbe) used() int {
	probe.mu.Lock()
	defer probe.mu.Unlock()

	return probe.calls
}

// newScriptedStructureAdapter is an adapter whose pooled factory must never
// run and whose dedicated reader is answer. It returns the observed attempts.
func newScriptedStructureAdapter(t *testing.T, answer func(attempt int) (selectedStructure, error)) (*Adapter, func() []scriptedAttempt) {
	t.Helper()

	if attachmentStructureHooks.scripted == nil {
		t.Fatal("the scripted dedicated-reader seam is absent")
	}

	adapter := &Adapter{
		config: ValidatedConfig{IMAP: IMAPConfig{CommandTimeoutMs: 2000}},
		gate:   make(chan struct{}, 1),
	}
	adapter.idKey[0] = 0x41
	adapter.factory = func(context.Context) (readSession, error) {
		t.Error("the dedicated operation opened a pooled session")
		return nil, errorCode(CodeBridgeUnreachable)
	}
	attempts := attachmentStructureHooks.scripted(adapter, answer)
	adapter.gate <- struct{}{}

	return adapter, attempts
}

func plainSelection() selectedStructure {
	return selectedStructure{
		Structure: &imap.BodyStructureSinglePart{Type: "application", Subtype: "pdf", Size: 10, Extended: &imap.BodyStructureSinglePartExt{Disposition: &imap.BodyStructureDisposition{Value: "attachment", Params: map[string]string{"filename": "a.pdf"}}}},
	}
}

func requireGateReleased(t *testing.T, adapter *Adapter) {
	t.Helper()

	if len(adapter.gate) != 1 {
		t.Fatal("the operation did not return the adapter gate")
	}
}

func TestAttachmentStructureAdapter(t *testing.T) {
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

		// The current ID returns exactly its own tuple and the projection.
		identifier := headerTestID(t, adapter, 9001, 102)
		result, err := callAttachmentStructure(t, ctx, adapter, identifier)
		if err != nil {
			t.Fatalf("current ID error = %v", err)
		}
		want := attachmentStructureResult{
			ID: identifier, Mailbox: "INBOX", UIDValidity: 9001, UID: 102,
			Attachments: []attachmentStructureView{{
				Metadata:          AttachmentMetadata{Filename: "report.pdf", ContentType: "application/octet-stream", Disposition: "attachment", DeclaredSize: 3000, HasDeclaredSize: true},
				HasTransferOctets: true, TransferOctets: 4096,
			}},
		}
		if !reflect.DeepEqual(result, want) {
			t.Fatalf("current ID result = %+v, want %+v", result, want)
		}
		if fetches := fetchesSince(server, 0); fetches != 1 {
			t.Fatalf("current ID issued %d FETCH commands", fetches)
		}

		// A stale generation is refused after the dedicated selection and
		// before any FETCH; the existence SEARCH may run.
		before := len(server.Commands())
		result, err = callAttachmentStructure(t, ctx, adapter, headerTestID(t, adapter, 9002, 102))
		requireStructureRefusal(t, "stale generation", result, err, CodeStaleMessageID)
		if fetches := fetchesSince(server, before); fetches != 0 {
			t.Fatalf("stale generation issued %d FETCH commands", fetches)
		}
		examined := false
		for _, command := range server.Commands()[before:] {
			examined = examined || command.Name == "EXAMINE"
		}
		if !examined {
			t.Fatal("stale generation was not decided on a fresh dedicated selection")
		}

		// A missing UID in the current generation is stale before any FETCH.
		before = len(server.Commands())
		result, err = callAttachmentStructure(t, ctx, adapter, headerTestID(t, adapter, 9001, 150))
		requireStructureRefusal(t, "missing UID", result, err, CodeStaleMessageID)
		if fetches := fetchesSince(server, before); fetches != 0 {
			t.Fatalf("missing UID issued %d FETCH commands", fetches)
		}

		// An ID from a previous process key never reaches the server.
		restarted := &Adapter{}
		restarted.idKey[0] = 0x7e
		before = len(server.Commands())
		result, err = callAttachmentStructure(t, ctx, adapter, headerTestID(t, restarted, 9001, 102))
		requireStructureRefusal(t, "restart-invalid ID", result, err, CodeStaleMessageID)
		if got := len(server.Commands()); got != before {
			t.Fatalf("restart-invalid ID issued %d commands", got-before)
		}

		// Every dedicated session was read-only and never pooled.
		if err := server.AssertReadOnlyCommands(); err != nil {
			t.Fatalf("read-only transcript: %v", err)
		}
		adapter.mu.Lock()
		pooled := adapter.session
		adapter.mu.Unlock()
		if pooled != nil {
			t.Fatal("the dedicated operation retained a pooled session")
		}
	})

	t.Run("plain_metadata_and_size", func(t *testing.T) {
		filename := testkit.BodyParam{Name: "filename", Value: "file.bin"}
		cases := []struct {
			name       string
			descriptor *testkit.BodyPart
			want       []attachmentStructureView
		}{
			{
				name:       "filename_before_name_absent_size",
				descriptor: plainDescriptor(512, []testkit.BodyParam{{Name: "name", Value: "type-name.bin"}}, &testkit.BodyDisposition{Value: "INLINE", Params: []testkit.BodyParam{filename}}),
				want:       []attachmentStructureView{{Metadata: AttachmentMetadata{Filename: "file.bin", ContentType: "application/octet-stream", Disposition: "inline"}, HasTransferOctets: true, TransferOctets: 512}},
			},
			{
				name:       "content_type_name_without_disposition",
				descriptor: plainDescriptor(77, []testkit.BodyParam{{Name: "name", Value: "type-name.bin"}}, nil),
				want:       []attachmentStructureView{{Metadata: AttachmentMetadata{Filename: "type-name.bin", ContentType: "application/octet-stream"}, HasTransferOctets: true, TransferOctets: 77}},
			},
			{
				name:       "attachment_disposition_without_name_zero_size",
				descriptor: plainDescriptor(0, nil, attachmentDisposition(testkit.BodyParam{Name: "size", Value: "0"})),
				want:       []attachmentStructureView{{Metadata: AttachmentMetadata{ContentType: "application/octet-stream", Disposition: "attachment", HasDeclaredSize: true}, HasTransferOctets: true}},
			},
			{
				name:       "positive_size",
				descriptor: plainDescriptor(4000, nil, attachmentDisposition(filename, testkit.BodyParam{Name: "size", Value: "2900"})),
				want:       []attachmentStructureView{{Metadata: AttachmentMetadata{Filename: "file.bin", ContentType: "application/octet-stream", Disposition: "attachment", DeclaredSize: 2900, HasDeclaredSize: true}, HasTransferOctets: true, TransferOctets: 4000}},
			},
			{
				name:       "negative_size",
				descriptor: plainDescriptor(9, nil, attachmentDisposition(filename, testkit.BodyParam{Name: "size", Value: "-1"})),
				want:       []attachmentStructureView{{Metadata: AttachmentMetadata{Filename: "file.bin", ContentType: "application/octet-stream", Disposition: "attachment"}, HasTransferOctets: true, TransferOctets: 9}},
			},
			{
				name:       "invalid_size",
				descriptor: plainDescriptor(9, nil, attachmentDisposition(filename, testkit.BodyParam{Name: "size", Value: "12kb"})),
				want:       []attachmentStructureView{{Metadata: AttachmentMetadata{Filename: "file.bin", ContentType: "application/octet-stream", Disposition: "attachment"}, HasTransferOctets: true, TransferOctets: 9}},
			},
			{
				name:       "int64_maximum_size",
				descriptor: plainDescriptor(4294967295, nil, attachmentDisposition(filename, testkit.BodyParam{Name: "size", Value: "9223372036854775807"})),
				want:       []attachmentStructureView{{Metadata: AttachmentMetadata{Filename: "file.bin", ContentType: "application/octet-stream", Disposition: "attachment", DeclaredSize: 9223372036854775807, HasDeclaredSize: true}, HasTransferOctets: true, TransferOctets: 4294967295}},
			},
			{
				name:       "text_leaf_with_name",
				descriptor: &testkit.BodyPart{Kind: testkit.BodyPartText, Type: "TEXT", Subtype: "CSV", Params: []testkit.BodyParam{{Name: "NAME", Value: "rows.csv"}}, Size: 31, Lines: 3},
				want:       []attachmentStructureView{{Metadata: AttachmentMetadata{Filename: "rows.csv", ContentType: "text/csv"}, HasTransferOctets: true, TransferOctets: 31}},
			},
			{
				name:       "plain_nonattachment_leaf",
				descriptor: &testkit.BodyPart{Kind: testkit.BodyPartText, Type: "text", Subtype: "plain", Size: 5, Lines: 1, Disposition: &testkit.BodyDisposition{Value: "inline"}},
				want:       []attachmentStructureView{},
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
			result, err := callAttachmentStructure(t, context.Background(), adapter, headerTestID(t, adapter, 9001, uid))
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

	t.Run("interim_subset", func(t *testing.T) {
		long := strings.Repeat("l", defaultMaxFilenameBytes+1)
		cases := []struct {
			name       string
			descriptor *testkit.BodyPart
		}{
			{name: "multipart", descriptor: &testkit.BodyPart{Kind: testkit.BodyPartMultipart, Subtype: "mixed", Children: []*testkit.BodyPart{
				{Kind: testkit.BodyPartText, Type: "text", Subtype: "plain", Size: 5, Lines: 1},
				plainDescriptor(64, nil, attachmentDisposition(testkit.BodyParam{Name: "filename", Value: "child.bin"})),
			}}},
			{name: "rfc2231_continued_filename", descriptor: plainDescriptor(8, nil, attachmentDisposition(testkit.BodyParam{Name: "filename*0", Value: "part-one"}, testkit.BodyParam{Name: "filename*1", Value: ".pdf"}))},
			{name: "rfc2231_extended_continued_name", descriptor: plainDescriptor(8, []testkit.BodyParam{{Name: "name*0*", Value: "utf-8''a%20b"}}, nil)},
			{name: "rfc2231_name_beside_plain_filename", descriptor: plainDescriptor(8, []testkit.BodyParam{{Name: "name*", Value: "utf-8''n.bin"}}, attachmentDisposition(testkit.BodyParam{Name: "filename", Value: "plain.bin"}))},
			{name: "long_filename", descriptor: plainDescriptor(8, nil, attachmentDisposition(testkit.BodyParam{Name: "filename", Value: long}))},
			{name: "long_name", descriptor: plainDescriptor(8, []testkit.BodyParam{{Name: "name", Value: long}}, nil)},
			// A bounded disposition filename does not mask an overlong
			// Content-Type name it takes precedence over.
			{name: "long_name_masked_by_filename", descriptor: plainDescriptor(8, []testkit.BodyParam{{Name: "name", Value: long}}, &testkit.BodyDisposition{Value: "attachment", Params: []testkit.BodyParam{{Name: "filename", Value: "short.bin"}}})},
			{name: "long_content_type", descriptor: &testkit.BodyPart{Kind: testkit.BodyPartBasic, Type: "application", Subtype: strings.Repeat("s", defaultMaxContentTypeBytes), Size: 8, Disposition: attachmentDisposition()}},
			{name: "long_disposition", descriptor: plainDescriptor(8, nil, &testkit.BodyDisposition{Value: strings.Repeat("d", defaultMaxDispositionBytes+1), Params: []testkit.BodyParam{{Name: "filename", Value: "x.bin"}}})},
			// Overlong fields of an unnamed non-attachment are refused, not
			// listed as a complete empty result.
			{name: "long_disposition_nonattachment", descriptor: plainDescriptor(8, nil, &testkit.BodyDisposition{Value: strings.Repeat("d", defaultMaxDispositionBytes+1)})},
			{name: "long_content_type_nonattachment", descriptor: &testkit.BodyPart{Kind: testkit.BodyPartBasic, Type: "application", Subtype: strings.Repeat("s", defaultMaxContentTypeBytes), Size: 8}},
		}

		messages := make([]string, 0, len(cases))
		descriptors := make([]*testkit.BodyPart, 0, len(cases))
		for index := range cases {
			messages = append(messages, "Subject: interim\r\n\r\nbody\r\n")
			descriptors = append(descriptors, cases[index].descriptor)
		}
		server, adapter := startTLSHeaderAdapter(t, testkit.StartTLS, testkit.Options{Messages: messages, BodyStructures: descriptors})

		for index, testCase := range cases {
			result, err := callAttachmentStructure(t, context.Background(), adapter, headerTestID(t, adapter, 9001, uint32(101+index)))
			requireStructureRefusal(t, testCase.name, result, err, CodeUnsupported)
		}
		if err := server.AssertReadOnlyCommands(); err != nil {
			t.Fatalf("read-only transcript: %v", err)
		}

		// The fixture cannot render an embedded message or invalid UTF-8,
		// so these are decided on the decoded structure the dedicated
		// session would return. A 200-byte filename or name of FF 61 pairs
		// repairs to 400 bytes, over the field limit.
		repairable := strings.Repeat("\xffa", 100)
		decoded := []bodyStructure{
			&imap.BodyStructureSinglePart{Type: "message", Subtype: "rfc822", Size: 90, MessageRFC822: &imap.BodyStructureMessageRFC822{BodyStructure: &imap.BodyStructureSinglePart{Type: "text", Subtype: "plain"}}},
			&imap.BodyStructureSinglePart{Type: "MESSAGE", Subtype: "GLOBAL", Size: 90},
			&imap.BodyStructureSinglePart{Type: "message", Subtype: "rfc822", Size: 90, Extended: &imap.BodyStructureSinglePartExt{Disposition: &imap.BodyStructureDisposition{Value: "attachment", Params: map[string]string{"filename": "fwd.eml"}}}},
			&imap.BodyStructureSinglePart{Type: "application", Subtype: "pdf", Size: 90, Extended: &imap.BodyStructureSinglePartExt{Disposition: &imap.BodyStructureDisposition{Value: "attachment", Params: map[string]string{"filename": repairable}}}},
			&imap.BodyStructureSinglePart{Type: "application", Subtype: "pdf", Size: 90, Params: map[string]string{"name": repairable}},
		}
		for index, structure := range decoded {
			selection := selectedStructure{Structure: structure}
			scripted, _ := newScriptedStructureAdapter(t, func(int) (selectedStructure, error) { return selection, nil })
			result, err := callAttachmentStructure(t, context.Background(), scripted, headerTestID(t, scripted, 9001, 7))
			requireStructureRefusal(t, fmt.Sprintf("decoded structure %d", index), result, err, CodeUnsupported)
		}
	})

	t.Run("gate_and_retry", func(t *testing.T) {
		ctx := context.Background()

		// A closed adapter refuses after acquiring the gate and before any
		// dedicated read.
		closed, attempts := newScriptedStructureAdapter(t, func(int) (selectedStructure, error) { return plainSelection(), nil })
		if err := closed.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
		result, err := callAttachmentStructure(t, ctx, closed, headerTestID(t, closed, 9001, 7))
		requireStructureRefusal(t, "closed adapter", result, err, CodeAdapterClosed)
		if len(attempts()) != 0 {
			t.Fatal("closed adapter dialed")
		}
		requireGateReleased(t, closed)

		// A cancellation queued behind the held gate never reads and does
		// not return a gate it never took.
		queued, attempts := newScriptedStructureAdapter(t, func(int) (selectedStructure, error) { return plainSelection(), nil })
		<-queued.gate
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		result, err = callAttachmentStructure(t, canceled, queued, headerTestID(t, queued, 9001, 7))
		requireStructureRefusal(t, "queued cancellation", result, err, CodeOperationCanceled)
		if len(attempts()) != 0 || len(queued.gate) != 0 {
			t.Fatal("queued cancellation read or returned a gate it did not hold")
		}
		queued.gate <- struct{}{}

		// Non-transport refusals of the first attempt are final, and a
		// pooled session already present is neither used nor replaced.
		controls := []struct {
			code     string
			attempts int
		}{
			{code: CodeStaleMessageID, attempts: 1},
			{code: CodeAuthentication, attempts: 1},
			{code: CodeIMAPProtocol, attempts: 1},
			{code: CodeBoundsExceeded, attempts: 1},
			{code: CodeBridgeUnreachable, attempts: 2},
		}
		for _, control := range controls {
			adapter, attempts := newScriptedStructureAdapter(t, func(int) (selectedStructure, error) {
				return selectedStructure{}, errorCode(control.code)
			})
			probe := &pooledProbe{}
			adapter.session = probe

			result, err := callAttachmentStructure(t, ctx, adapter, headerTestID(t, adapter, 9001, 7))
			requireStructureRefusal(t, control.code, result, err, control.code)
			if len(attempts()) != control.attempts {
				t.Fatalf("%s made %d attempts, want %d", control.code, len(attempts()), control.attempts)
			}
			if adapter.session != readSession(probe) || probe.used() != 0 {
				t.Fatalf("%s used or replaced the pooled session", control.code)
			}
			requireGateReleased(t, adapter)
		}

		// Cancellation during the first attempt is not replayed even though
		// the attempt failed in the transport category.
		parent, cancelParent := context.WithCancel(ctx)
		adapter, attempts := newScriptedStructureAdapter(t, func(int) (selectedStructure, error) {
			cancelParent()
			return selectedStructure{}, errorCode(CodeBridgeUnreachable)
		})
		result, err = callAttachmentStructure(t, parent, adapter, headerTestID(t, adapter, 9001, 7))
		requireStructureRefusal(t, "cancel", result, err, CodeOperationCanceled)
		if len(attempts()) != 1 {
			t.Fatalf("cancel made %d attempts", len(attempts()))
		}
		requireGateReleased(t, adapter)

		// One transport failure is replayed once under the same outer
		// operation context, and only the second attempt's data is used.
		adapter, attempts = newScriptedStructureAdapter(t, func(attempt int) (selectedStructure, error) {
			if attempt == 0 {
				return selectedStructure{Snapshot: mailboxSnapshot{UIDValidity: 9001}}, errorCode(CodeBridgeUnreachable)
			}
			return plainSelection(), nil
		})
		probe := &pooledProbe{}
		adapter.session = probe
		identifier := headerTestID(t, adapter, 9001, 7)
		result, err = callAttachmentStructure(t, ctx, adapter, identifier)
		finished := time.Now()
		if err != nil || result.ID != identifier || len(result.Attachments) != 1 || result.Attachments[0].Metadata.Filename != "a.pdf" {
			t.Fatalf("replayed result = %+v, %v", result, err)
		}
		observed := attempts()
		if len(observed) != 2 || observed[0].ctx != observed[1].ctx {
			t.Fatal("the replay did not reuse the one outer operation context")
		}
		deadline, ok := observed[0].ctx.Deadline()
		if !ok || deadline.After(finished.Add(2*time.Second)) || observed[0].ctx.Err() == nil {
			t.Fatal("the outer operation context was unbounded, extended or outlived the operation")
		}
		for _, attempt := range observed {
			if attempt.mailbox != "INBOX" || attempt.uid != 7 || attempt.uidValidity != 9001 {
				t.Fatalf("dedicated request = %+v, want the ID's bound selection", attempt)
			}
		}
		if adapter.session != readSession(probe) || probe.used() != 0 {
			t.Fatal("the replay used or replaced the pooled session")
		}
		requireGateReleased(t, adapter)

		// The real stack: the named selectionCutConn seam ends the first
		// dedicated transport after partial EXAMINE data, so the quarantine
		// and session classify the failure; the second fresh session reads
		// the structure.
		server, real := startTLSHeaderAdapter(t, testkit.StartTLS, testkit.Options{
			Messages:       []string{"Subject: retry\r\n\r\nbody\r\n"},
			BodyStructures: []*testkit.BodyPart{plainDescriptor(300, nil, attachmentDisposition(testkit.BodyParam{Name: "filename", Value: "retry.bin"}))},
		})
		var mu sync.Mutex
		var cuts []*selectionCutConn
		wrapStructureTransport(t, real, func(attempt int, transport net.Conn) net.Conn {
			if attempt != 0 {
				return transport
			}
			cut := &selectionCutConn{Conn: transport}
			mu.Lock()
			cuts = append(cuts, cut)
			mu.Unlock()
			return cut
		})
		probe = &pooledProbe{}
		real.session = probe

		identifier = headerTestID(t, real, 9001, 101)
		result, err = callAttachmentStructure(t, ctx, real, identifier)
		if err != nil {
			t.Fatalf("real replay error = %v", err)
		}
		if result.ID != identifier || result.UID != 101 || len(result.Attachments) != 1 || result.Attachments[0].Metadata.Filename != "retry.bin" || result.Attachments[0].TransferOctets != 300 {
			t.Fatalf("real replay result = %+v", result)
		}
		mu.Lock()
		cutCount := len(cuts)
		mu.Unlock()
		if cutCount != 1 || !cuts[0].wasCut() {
			t.Fatal("the first attempt was not cut after partial selection data")
		}

		connections := map[int][]string{}
		var order []int
		for _, command := range server.Commands() {
			if _, seen := connections[command.ConnectionID]; !seen {
				order = append(order, command.ConnectionID)
			}
			connections[command.ConnectionID] = append(connections[command.ConnectionID], strings.ToUpper(command.Raw))
		}
		if len(order) != 2 {
			t.Fatalf("real replay used %d connections, want two fresh dedicated sessions", len(order))
		}
		firstAttempt := strings.Join(connections[order[0]], "\n")
		if !strings.Contains(firstAttempt, " EXAMINE ") || strings.Contains(firstAttempt, " FETCH ") {
			t.Fatalf("first attempt transcript = %q", connections[order[0]])
		}
		if fetches := strings.Count(strings.Join(connections[order[1]], "\n"), " FETCH "); fetches != 1 {
			t.Fatalf("second attempt issued %d FETCH commands", fetches)
		}
		if real.session != readSession(probe) || probe.used() != 0 {
			t.Fatal("the real replay used or replaced the pooled session")
		}
		requireGateReleased(t, real)

		// Close after the operation stays bounded and closes the untouched
		// pooled session exactly once.
		if err := real.Close(); err != nil || probe.used() != 1 {
			t.Fatalf("Close() = %v with %d pooled calls", err, probe.used())
		}
	})
}

// selectionCutConn is the private transport seam that produces a transport
// failure after partial selection data: once the client sends EXAMINE, it
// passes the first complete server response line through and then closes the
// transport, so the quarantine sees the source fail between responses.
type selectionCutConn struct {
	net.Conn

	mu    sync.Mutex
	armed bool
	cut   bool
}

func (connection *selectionCutConn) Write(buffer []byte) (int, error) {
	if bytes.Contains(bytes.ToUpper(buffer), []byte(" EXAMINE ")) {
		connection.mu.Lock()
		connection.armed = true
		connection.mu.Unlock()
	}

	return connection.Conn.Write(buffer)
}

func (connection *selectionCutConn) Read(buffer []byte) (int, error) {
	if connection.wasCut() {
		_ = connection.Conn.Close()
		return 0, io.EOF
	}

	count, err := connection.Conn.Read(buffer)

	connection.mu.Lock()
	defer connection.mu.Unlock()
	if connection.armed {
		if end := bytes.Index(buffer[:count], []byte("\r\n")); end >= 0 {
			count = end + 2
			connection.cut = true
		}
	}
	return count, err
}

func (connection *selectionCutConn) wasCut() bool {
	connection.mu.Lock()
	defer connection.mu.Unlock()

	return connection.cut
}
