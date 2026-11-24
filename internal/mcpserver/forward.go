package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
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

type preparedForward struct {
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
	server.AddTool(&mcp.Tool{Name: def.name, Description: def.description, InputSchema: def.schema,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(false)},
	}, makeHandler(def, options))
}

func runPrepareForward(ctx context.Context, options Options, args json.RawMessage) (any, string) {
	var in forwardInput
	if !strictjson.DecodeObject(args, stdioframe.MaxFrameBytes, &in) || !validMessageIdentifier(in.MessageID) || !plainMailbox(in.To) {
		return nil, errInvalidArgument
	}
	authored := strings.ReplaceAll(in.Text, "\r\n", "\n")
	if strings.ContainsFunc(authored, func(r rune) bool {
		return r == utf8.RuneError || (unicode.IsControl(r) && r != '\t' && r != '\n')
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
	pending := &preparedForward{forwardResult: result, message: message, expires: now.Add(2 * time.Minute), connectionClosed: source.ConnectionClosed}

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
