package mcpserver

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"mime/quotedprintable"
	"net/mail"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/forwardpolicy"
	"github.com/wevial/croton-mcp/internal/stdioframe"
	"github.com/wevial/croton-mcp/internal/strictjson"
)

const maxForwardAddressBytes = 254

type forwardInput struct {
	MessageID string `json:"messageId"`
	To        string `json:"to"`
	Text      string `json:"text"`
	TextOnly  bool   `json:"text_only"`
}

type forwardPreview struct {
	From      string                  `json:"from"`
	To        string                  `json:"to"`
	Subject   string                  `json:"subject"`
	Body      string                  `json:"body"`
	Omissions bridge.ForwardOmissions `json:"omissions"`
}

type forwardResult struct {
	Handle  string         `json:"handle"`
	Preview forwardPreview `json:"preview"`
}

type forwardSessionKey struct{}

type preparedForward struct {
	session   *mcp.ServerSession
	challenge string
	forwardResult
	message          string
	expires          time.Time
	connectionClosed <-chan struct{}
}

// ForwardPreviews retains one complete prepared message for one stdio session.
type ForwardPreviews struct {
	adapter *bridge.Adapter
	mu      sync.Mutex
	current *preparedForward
	timer   *time.Timer
}

func NewForwardPreviews(adapter *bridge.Adapter) *ForwardPreviews {
	return &ForwardPreviews{adapter: adapter}
}

func registerForward(server *mcp.Server, options Options) {
	def := toolDefinition{
		name:        "prepare_forward",
		description: "Preview one complete inline plain-text forward to an explicit recipient. Set text_only to consent to omitting files and attached messages.",
		schema: objectSchema(map[string]json.RawMessage{
			"messageId": stringSchema(maxMessageIDArgumentBytes),
			"to":        stringSchema(maxForwardAddressBytes),
			"text":      stringSchema(stdioframe.MaxFrameBytes),
			"text_only": booleanSchema(),
		}, []string{"messageId", "to"}),
		run: runPrepareForward,
	}
	handler := makeHandler(def, options)
	server.AddTool(&mcp.Tool{Name: def.name, Description: def.description, InputSchema: def.schema,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(false)},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return handler(context.WithValue(ctx, forwardSessionKey{}, req.Session), req)
	})
}

func runPrepareForward(ctx context.Context, options Options, args json.RawMessage) (any, string) {
	var in forwardInput
	if !strictjson.DecodeObject(args, stdioframe.MaxFrameBytes, &in) || !validMessageIdentifier(in.MessageID) || !plainMailbox(in.To) {
		return nil, errInvalidArgument
	}
	// encoding/json repairs malformed UTF-8 and unpaired surrogate escapes.
	if strings.ContainsRune(in.Text, utf8.RuneError) && !jsontext.Value(args).IsValid() {
		return nil, errInvalidArgument
	}

	authored := strings.ReplaceAll(in.Text, "\r\n", "\n")
	if strings.ContainsFunc(authored, func(r rune) bool {
		return unicode.IsControl(r) && r != '\t' && r != '\n'
	}) {
		return nil, errInvalidArgument
	}

	store := options.Forward
	source, err := store.adapter.ReadForwardSource(ctx, in.MessageID, in.TextOnly)
	if err != nil {
		return nil, mapAdapterError(err)
	}
	if !plainMailbox(source.Login) {
		return nil, errUnsupported
	}

	if authored != "" {
		authored += "\n\n"
	}
	body := fmt.Sprintf("%s---------- Forwarded message ----------\nFrom: %s\nDate: %s\nSubject: %s\n\n%s",
		authored, source.From, source.Date, source.Subject, source.Text)

	result := forwardResult{Handle: rand.Text(), Preview: forwardPreview{
		From: source.Login, To: in.To, Subject: strings.TrimSpace("Fwd: " + strings.Join(strings.Fields(source.Subject), " ")),
		Body: strings.ReplaceAll(body, "\n", "\r\n"), Omissions: source.Omissions,
	}}
	if !fitsForwardFrame(result) {
		return nil, errBoundsExceeded
	}

	now := time.Now()
	message := serializeForward(result.Preview, now)
	if len(message) > bridge.MaxForwardMIMEBytes {
		return nil, errBoundsExceeded
	}
	session, _ := ctx.Value(forwardSessionKey{}).(*mcp.ServerSession)
	pending := &preparedForward{session: session, forwardResult: result, message: message, expires: now.Add(2 * time.Minute), connectionClosed: source.ConnectionClosed}

	store.mu.Lock()
	defer store.mu.Unlock()
	if ctx.Err() != nil {
		return nil, errCanceled
	}
	if !pending.valid(time.Now()) {
		return nil, errUnavailable
	}

	if store.timer != nil {
		store.timer.Stop()
	}
	store.current = pending
	handle := result.Handle
	store.timer = time.AfterFunc(time.Until(pending.expires), func() {
		store.mu.Lock()
		defer store.mu.Unlock()
		if store.current != nil && store.current.Handle == handle {
			store.current = nil
		}
	})

	return result, ""
}

var mailboxDomainLabel = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

func plainMailbox(value string) bool {
	if value == "" || len(value) > maxForwardAddressBytes || strings.ContainsFunc(value, func(r rune) bool { return r <= ' ' || r >= 127 }) {
		return false
	}
	address, err := mail.ParseAddress(value)
	if err != nil || address.Name != "" || address.Address != value {
		return false
	}
	local, domain, ok := strings.Cut(value, "@")
	if !ok || len(local) > 64 {
		return false
	}

	if strings.HasPrefix(domain, "[") {
		literal := domain[1 : len(domain)-1]
		ip, ipv6 := strings.CutPrefix(literal, "IPv6:")
		addr, err := netip.ParseAddr(ip)
		return err == nil && addr.Zone() == "" && addr.Is6() == ipv6
	}
	for label := range strings.SplitSeq(domain, ".") {
		if !mailboxDomainLabel.MatchString(label) {
			return false
		}
	}

	return true
}

// MaxFrameBytes includes the canonical call's terminating newline.
func fitsForwardFrame(result forwardResult) bool {
	args, err := json.Marshal(result)
	const envelope = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"send_forward","arguments":`
	return err == nil && len(envelope)+len(args)+len("}}\n") <= stdioframe.MaxFrameBytes
}

func (pending *preparedForward) valid(now time.Time) bool {
	if pending == nil || !now.Before(pending.expires) || pending.connectionClosed == nil {
		return false
	}
	select {
	case <-pending.connectionClosed:
		return false
	default:
		return true
	}
}

// Keep the approved MIME bytes so lookup preserves the original envelope and Message-ID.
func (store *ForwardPreviews) lookup(approval forwardResult, now time.Time) (string, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()

	pending := store.current
	if !pending.valid(now) {
		store.current = nil
		return "", false
	}
	if pending.forwardResult != approval {
		return "", false
	}
	return pending.message, true
}

// openApproved checks the form, atomically spends the preview and opens the
// authenticated transport. The MCP catalog has no submission handler.
func (store *ForwardPreviews) openApproved(ctx context.Context, cfg bridge.ForwardConfig, req *mcp.CallToolRequest) (*bridge.ForwardSMTP, *preparedForward, *mcp.CallToolResult) {
	pending, result := store.approval(cfg, req)
	if result != nil {
		return nil, nil, result
	}

	smtp, err := store.adapter.OpenForwardSMTP(ctx, cfg, pending.Preview.From, pending.connectionClosed, func() error {
		return store.reserve(ctx, cfg.StateDir, pending)
	})
	if err != nil {
		return nil, nil, errorResult(errUnavailable)
	}
	return smtp, pending, nil
}

// approval handles both rounds of a form elicitation. The SDK carries the
// input request for current clients and handles the round trip for legacy ones.
// The challenge belongs to just this preview, session and form.
func (store *ForwardPreviews) approval(cfg bridge.ForwardConfig, request *mcp.CallToolRequest) (*preparedForward, *mcp.CallToolResult) {
	if !cfg.Enabled || request == nil || request.Session == nil || request.Params == nil {
		return nil, errorResult(errUnavailable)
	}
	caps := request.ClientCapabilities()
	if caps == nil || caps.Elicitation == nil || caps.Elicitation.Form == nil {
		return nil, errorResult(errUnsupported)
	}

	params := request.Params
	if len(params.Arguments) > stdioframe.MaxFrameBytes {
		return nil, errorResult(errBoundsExceeded)
	}
	args := jsontext.Value(bytes.Clone(params.Arguments))
	if err := args.Format(jsontext.ReorderRawObjects(true)); err != nil {
		return nil, errorResult(errInvalidArgument)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	pending := store.current
	if !pending.valid(time.Now()) || pending.session != request.Session {
		return nil, errorResult(errUnavailable)
	}
	// Compare the whole JSON tree including required zero-valued fields.
	// Formatting preserves string values and number syntax and removes only
	// insignificant object order, whitespace and escape differences.
	stored, err := json.Marshal(pending.forwardResult)
	want := jsontext.Value(stored)
	if err != nil || want.Format(jsontext.ReorderRawObjects(true)) != nil || !bytes.Equal(want, args) {
		return nil, errorResult(errInvalidArgument)
	}

	if params.InputResponses == nil && params.RequestState == "" {
		pending.challenge = rand.Text()
		preview, _ := json.Marshal(pending.Preview)
		return nil, &mcp.CallToolResult{
			InputRequests: mcp.InputRequestMap{"confirmation": &mcp.ElicitParams{
				Mode: "form", Message: "Approve this one forward? Review the complete preview.\n" + string(preview),
				RequestedSchema: objectSchema(map[string]json.RawMessage{"confirm": booleanSchema()}, []string{"confirm"}),
			}},
			RequestState: pending.challenge,
		}
	}

	if pending.challenge == "" || pending.challenge != params.RequestState {
		return nil, errorResult(errInvalidArgument)
	}
	pending.challenge = ""
	response, ok := params.InputResponses["confirmation"].(*mcp.ElicitResult)
	if !ok || response == nil || len(params.InputResponses) != 1 || response.Action != "accept" ||
		len(response.Content) != 1 || response.Content["confirm"] != true {
		return nil, errorResult(errInvalidArgument)
	}
	return pending, nil
}

// reserve consumes the exact prepared message after the adapter has rechecked
// its login. A committed slot survives all subsequent transport outcomes.
func (store *ForwardPreviews) reserve(ctx context.Context, dir string, pending *preparedForward) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if ctx.Err() != nil || pending != store.current || !pending.valid(time.Now()) {
		return forwardpolicy.ErrUnavailable
	}

	if err := forwardpolicy.Reserve(dir, pending.Preview.From); err != nil {
		return err
	}
	store.current = nil
	store.timer.Stop()
	if ctx.Err() != nil || !pending.valid(time.Now()) {
		return forwardpolicy.ErrUnavailable
	}
	return nil
}

func serializeForward(preview forwardPreview, now time.Time) string {
	var wire strings.Builder
	fmt.Fprintf(&wire, "From: %s\r\nTo: %s\r\nDate: %s\r\nMessage-ID: <%s@croton.invalid>\r\nSubject: ",
		preview.From, preview.To, now.Format(time.RFC1123Z), rand.Text())
	// RFC 2047 words let even an unbroken ASCII subject fold within the 998-octet limit.
	subject := preview.Subject
	for len(subject) > 0 {
		end := min(45, len(subject))
		for end < len(subject) && !utf8.RuneStart(subject[end]) {
			end--
		}
		fmt.Fprintf(&wire, "=?UTF-8?B?%s?=", base64.StdEncoding.EncodeToString([]byte(subject[:end])))
		subject = subject[end:]
		if subject != "" {
			wire.WriteString("\r\n ")
		}
	}
	wire.WriteString("\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")

	qp := quotedprintable.NewWriter(&wire)
	_, _ = qp.Write([]byte(preview.Body))
	_ = qp.Close()
	return wire.String()
}
