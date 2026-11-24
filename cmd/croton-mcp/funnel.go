package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wevial/croton-mcp/internal/owneroauth"
	"github.com/wevial/croton-mcp/internal/strictjson"
)

const maxStatusBytes = 4 << 20
const funnelPortFlag = "--https=443"
const warningPrefix = `Warning: client version "`

var versionWarning = regexp.MustCompile(`^Warning: client version "[^"\r\n]+" != tailscaled server version "[^"\r\n]+"\n$`)

var (
	errStatus     = errors.New("cannot verify Tailscale status. Run tailscale status, tailscale serve status and tailscale funnel status before retrying")
	errChanged    = errors.New("the Tailscale configuration changed. Inspect the node and reserve port 443 exclusively before retrying")
	errOwnership  = errors.New("warning: could not confirm Funnel route ownership. Inspect port 443 manually")
	errEnablement = errors.New("unexpected Tailscale output. Check HTTPS and Funnel eligibility before retrying")
	errDeclined   = errors.New("operator did not confirm Funnel publication")
)

type funnelNode struct {
	host, id string
}

type tailscaleFunnel struct {
	node              funnelNode
	cli, target       string
	before, published string // canonical ServeConfig, including other ports
}

// Tailscale returns the same ServeConfig from both status commands. Services
// have their own virtual IPs and are included in the ownership comparison.
type serveConfig struct {
	TCP         map[string]map[string]any `json:",omitempty"`
	Web         map[string]map[string]any `json:",omitempty"`
	AllowFunnel map[string]*bool          `json:",omitempty"`
	Foreground  map[string]*serveConfig   `json:",omitempty"`
	Services    map[string]map[string]any `json:",omitempty"`
}

func (sc serveConfig) snapshot() string {
	// All values came from JSON or the literal route below.
	data, _ := json.Marshal(sc)
	return string(data)
}

func (sc serveConfig) withRoute(host, target string) string {
	if sc.TCP == nil {
		sc.TCP = make(map[string]map[string]any)
	}
	if sc.Web == nil {
		sc.Web = make(map[string]map[string]any)
	}
	if sc.AllowFunnel == nil {
		sc.AllowFunnel = make(map[string]*bool)
	}

	hp := host + ":443"
	sc.TCP["443"] = map[string]any{"HTTPS": true}
	sc.Web[hp] = map[string]any{"Handlers": map[string]any{"/": map[string]any{"Proxy": target}}}
	sc.AllowFunnel[hp] = new(true)
	return sc.snapshot()
}

func portNumber(s string) (uint16, bool) {
	n, err := strconv.ParseUint(s, 10, 16)
	return uint16(n), err == nil && n != 0 && strconv.FormatUint(n, 10) == s
}

func checkServePort(port, kind string, valid bool) error {
	n, ok := portNumber(port)
	if !valid || !ok {
		return errStatus
	}
	if n == 443 {
		return fmt.Errorf("%s configuration occupies port 443. Move it to another port or use a dedicated node", kind)
	}

	return nil
}

func checkServeHostPort(hp, kind string, hasHandler bool) error {
	host, port, err := net.SplitHostPort(hp)
	return checkServePort(port, kind, err == nil && host != "" && hasHandler)
}

func validServeWeb(web map[string]any) bool {
	mounts, _ := web["Handlers"].(map[string]any)
	if len(web) != 1 || len(mounts) == 0 {
		return false
	}

	for mount, value := range mounts {
		if !strings.HasPrefix(mount, "/") {
			return false
		}

		handler, _ := value.(map[string]any)
		targets := 0
		for key, field := range handler {
			switch key {
			case "Path", "Proxy", "Text", "Redirect":
				if target, ok := field.(string); !ok || target == "" {
					return false
				}
				targets++
			case "AcceptAppCaps":
				caps, ok := field.([]any)
				if !ok {
					return false
				}
				for _, capability := range caps {
					if _, ok := capability.(string); !ok {
						return false
					}
				}
			default:
				return false
			}
		}
		if targets != 1 {
			return false
		}
	}

	return true
}

func (sc serveConfig) checkFree() error {
	for session, fg := range sc.Foreground {
		if session == "" || fg == nil || len(fg.Foreground) != 0 {
			return errStatus
		}
		if err := fg.checkFree(); err != nil {
			return fmt.Errorf("foreground Serve/Funnel: %w", err)
		}
	}

	for port, handler := range sc.TCP {
		if err := checkServePort(port, "Serve TCP", handler != nil); err != nil {
			return err
		}
	}

	for hp, web := range sc.Web {
		if err := checkServeHostPort(hp, "Serve Web", validServeWeb(web)); err != nil {
			return err
		}
	}
	for hp, allowed := range sc.AllowFunnel {
		if err := checkServeHostPort(hp, "AllowFunnel", allowed != nil); err != nil {
			return err
		}
	}

	return nil
}

// Cancel on unexpected mutation output because an enablement prompt can wait for external approval.
type cliOutput struct {
	data          bytes.Buffer
	cancel        context.CancelFunc
	successPrefix []byte
	warningOnly   bool
	err           error
}

func (out *cliOutput) Write(p []byte) (int, error) {
	if out.warningOnly && out.data.Len()+len(p) > 4096 {
		out.err = errEnablement
	} else if out.data.Len()+len(p) > maxStatusBytes {
		out.err = errStatus
	} else {
		out.data.Write(p)
		if prefix := out.successPrefix; prefix != nil {
			got := out.data.Bytes()
			if (len(prefix) == 0 && len(got) != 0) || (!bytes.HasPrefix(got, prefix) && !bytes.HasPrefix(prefix, got)) {
				out.err = errEnablement
			}
			if out.warningOnly && bytes.ContainsRune(got, '\n') && !versionWarning.Match(got) {
				out.err = errEnablement
			}
		}
	}
	if out.err != nil {
		out.cancel()
		return 0, out.err
	}
	return len(p), nil
}

func (f *tailscaleFunnel) command(ctx context.Context, successPrefix []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	out := &cliOutput{cancel: cancel, successPrefix: successPrefix}
	warnings := &cliOutput{cancel: cancel, successPrefix: []byte(warningPrefix), warningOnly: true}
	cmd := exec.CommandContext(ctx, f.cli, args...)
	cmd.WaitDelay = time.Second
	cmd.Stdout = out
	if successPrefix != nil {
		cmd.Stderr = warnings
	}

	err := cmd.Run()
	if out.err != nil {
		return nil, out.err
	}
	if warnings.err != nil || (warnings.data.Len() != 0 && !versionWarning.Match(warnings.data.Bytes())) {
		return nil, errEnablement
	}
	if err != nil {
		return nil, errors.New("the Tailscale CLI command failed. Inspect Tailscale status and CLI permissions")
	}
	return out.data.Bytes(), nil
}

// The JSON decoder rejects exact duplicates in every object. Also reject
// case aliases in schema objects. Tailscale's maps have data keys, including
// case-sensitive HTTP mounts and foreground session IDs.
func checkServeKeys(dec *jsontext.Decoder, dataKeys bool) bool {
	token, err := dec.ReadToken()
	if err != nil || dec.StackDepth() > 32 {
		return false
	}

	switch token.Kind() {
	case '{':
		var names []string
		for dec.PeekKind() != '}' {
			name, err := dec.ReadToken()
			if err != nil {
				return false
			}
			key := name.String()
			if !dataKeys {
				for _, previous := range names {
					if strings.EqualFold(key, previous) {
						return false
					}
				}
				names = append(names, key)
			}

			childMap := !dataKeys && slices.Contains([]string{"TCP", "Web", "Services", "AllowFunnel", "Foreground", "Handlers"}, key)
			if !checkServeKeys(dec, childMap) {
				return false
			}
		}
	case '[':
		for dec.PeekKind() != ']' {
			if !checkServeKeys(dec, false) {
				return false
			}
		}
	default:
		return true
	}
	_, err = dec.ReadToken()
	return err == nil
}

func (f *tailscaleFunnel) config(ctx context.Context) (serveConfig, error) {
	var configs [2]serveConfig
	for i, subcommand := range []string{"serve", "funnel"} {
		data, err := f.command(ctx, nil, subcommand, "status", "--json")
		dec := jsontext.NewDecoder(bytes.NewReader(data))
		if err != nil || dec.PeekKind() != '{' || !checkServeKeys(dec, false) ||
			jsonv2.Unmarshal(data, &configs[i], jsonv2.RejectUnknownMembers(true)) != nil {
			return serveConfig{}, errStatus
		}
	}
	if configs[0].snapshot() != configs[1].snapshot() {
		return serveConfig{}, errChanged
	}
	return configs[0], nil
}

func (f *tailscaleFunnel) checkVersion(ctx context.Context) error {
	data, err := f.command(ctx, nil, "version")
	if err != nil {
		return errors.New("cannot read Tailscale CLI version. Check that Tailscale 1.104.5 or later is available")
	}

	line, _, _ := strings.Cut(string(data), "\n")
	parts := strings.Split(strings.TrimSuffix(line, "\r"), ".")
	var version [3]int
	if len(parts) == len(version) {
		for i, part := range parts {
			n, err := strconv.Atoi(part)
			if err != nil || n < 0 || strconv.Itoa(n) != part {
				return errors.New("unrecognized Tailscale CLI version. Use release 1.104.5 or later")
			}
			version[i] = n
		}
		if slices.Compare(version[:], []int{1, 104, 5}) >= 0 {
			return nil
		}
	}
	return errors.New("use Tailscale CLI 1.104.5 or later")
}

func allows443(caps map[string][]json.RawMessage) bool {
	const base = "https://tailscale.com/cap/funnel-ports"
	found, allowed := false, false
	for key := range caps {
		if !strings.HasPrefix(key, base) {
			continue
		}
		query, ok := strings.CutPrefix(key, base+"?")
		q, err := url.ParseQuery(query)
		if found || !ok || err != nil || len(q) != 1 || len(q["ports"]) != 1 {
			return false
		}

		found = true
		for spec := range strings.SplitSeq(q.Get("ports"), ",") {
			first, last, span := strings.Cut(spec, "-")
			if !span {
				last = first
			}
			lo, validFirst := portNumber(first)
			hi, validLast := portNumber(last)
			if !validFirst || !validLast || hi < lo {
				return false
			}
			allowed = allowed || (lo <= 443 && 443 <= hi)
		}
	}
	return allowed
}

var nodeDNSLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// Use exact identity and eligibility fields, keeping peer and user records out of diagnostics.
func (f *tailscaleFunnel) currentNode(ctx context.Context, requireEligibility bool) (funnelNode, error) {
	var status, self map[string]json.RawMessage
	var state, dnsName string
	var node funnelNode
	data, err := f.command(ctx, nil, "status", "--json")
	if err != nil || !strictjson.ValidateObject(data, maxStatusBytes) || json.Unmarshal(data, &status) != nil ||
		json.Unmarshal(status["Self"], &self) != nil || json.Unmarshal(status["BackendState"], &state) != nil ||
		json.Unmarshal(self["DNSName"], &dnsName) != nil {
		return node, errStatus
	}
	if json.Unmarshal(self["ID"], &node.id) != nil || strings.TrimSpace(node.id) == "" {
		return node, errStatus
	}

	node.host = strings.TrimSuffix(dnsName, ".")
	if state != "Running" || len(node.host) > 253 || !strings.Contains(node.host, ".") || ipv4Suffix.MatchString(node.host) {
		return node, errStatus
	}
	for label := range strings.SplitSeq(node.host, ".") {
		if !nodeDNSLabel.MatchString(label) {
			return node, errStatus
		}
	}
	if !requireEligibility {
		return node, nil
	}

	var certDomains []string
	if json.Unmarshal(status["CertDomains"], &certDomains) != nil || !slices.Contains(certDomains, node.host) {
		return node, errors.New("check that Tailscale has an HTTPS certificate domain for this node")
	}

	var caps map[string][]json.RawMessage
	if json.Unmarshal(self["CapMap"], &caps) != nil {
		return node, errStatus
	}
	_, https := caps["https"]
	_, funnel := caps["funnel"]
	if !https || !funnel || !allows443(caps) {
		return node, errors.New("check the node's HTTPS and Funnel eligibility for port 443 with the tailnet administrator")
	}
	return node, nil
}

func (f *tailscaleFunnel) preflight(ctx context.Context) error {
	if err := f.checkVersion(ctx); err != nil {
		return err
	}
	node, err := f.currentNode(ctx, true)
	if err != nil {
		return err
	}

	sc, err := f.config(ctx)
	if err != nil {
		return err
	}
	if err := sc.checkFree(); err != nil {
		return err
	}

	f.node = node
	f.before = sc.snapshot()
	f.published = sc.withRoute(node.host, f.target)
	return nil
}

func confirmFunnel(ctx context.Context, host, target string) error {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		return errors.New("an owner terminal is required to approve Funnel publication")
	}
	defer tty.Close()

	return readFunnelApproval(ctx, tty, tty, host, target)
}

func readFunnelApproval(ctx context.Context, r io.Reader, w io.Writer, host, target string) error {
	_, err := fmt.Fprintf(w, "Publish https://%s to the internet? All routes on port 443 will be public.\n"+
		"Root proxy / -> %s\nCroton routes: /mcp, %s, /.well-known/oauth-authorization-server, "+
		"/register, /authorize, /token, /owner/pair, /owner/signin, /owner/finish\n"+
		"Confirm you have exclusive operator control of port 443 until Croton stops. Type yes to publish: ",
		host, target, owneroauth.ResourceMetadataPath)
	if err != nil {
		return errDeclined
	}

	answer := make(chan bool, 1)
	go func() {
		line, err := bufio.NewReader(io.LimitReader(r, 8)).ReadString('\n')
		answer <- err == nil && (line == "yes\n" || line == "yes\r\n")
	}()
	select {
	case <-ctx.Done():
	case yes := <-answer:
		if !yes {
			return errDeclined
		}
	}
	return ctx.Err()
}

func approveFunnel(ctx context.Context, address netip.AddrPort, confirm func(context.Context, string, string) error) (*tailscaleFunnel, error) {
	if ip := address.Addr(); ip.String() != "127.0.0.1" && ip != netip.IPv6Loopback() {
		return nil, fmt.Errorf("use a loopback proxy target for Tailscale Funnel. Set --bind=127.0.0.1:%d", address.Port())
	}

	cli, err := exec.LookPath("tailscale")
	if err != nil {
		return nil, errors.New("cannot find the Tailscale CLI. Check PATH for Tailscale 1.104.5 or later")
	}

	approved := tailscaleFunnel{cli: cli, target: "http://" + address.String()}
	if err := approved.preflight(ctx); err != nil {
		return nil, err
	}
	if err := confirm(ctx, approved.node.host, approved.target); err != nil {
		return nil, err
	}

	current := approved
	if err := current.preflight(ctx); err != nil {
		return nil, err
	}
	if current != approved {
		return nil, errChanged
	}
	return &current, nil
}

func (f *tailscaleFunnel) remove() error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	node, err := f.currentNode(ctx, false)
	if err != nil || node != f.node {
		return errOwnership
	}
	sc, err := f.config(ctx)
	if err != nil || sc.snapshot() != f.published {
		return errOwnership
	}

	_, offErr := f.command(ctx, []byte{}, "funnel", funnelPortFlag, "off")
	sc, err = f.config(ctx)
	if offErr != nil || err != nil || sc.snapshot() != f.before {
		return errors.New("warning: could not verify Funnel removal. Inspect port 443 manually")
	}
	return nil
}

var funnelProbeTransport http.RoundTripper = &http.Transport{DisableKeepAlives: true}

func (f *tailscaleFunnel) serve(ctx context.Context, listener net.Listener, handler http.Handler) (err error) {
	// Keep the protected listener bound until cleanup has inspected the route.
	backend, stopBackend := context.WithCancel(context.WithoutCancel(ctx))
	active, stopPublish := context.WithCancel(ctx)
	defer stopPublish()
	done := make(chan error, 1)
	go func() {
		done <- runHTTP(backend, listener, handler)
		stopPublish()
	}()
	defer func() {
		stopBackend()
		err = errors.Join(err, <-done)
	}()

	req, err := http.NewRequestWithContext(active, http.MethodGet, f.target+"/mcp", nil)
	if err != nil {
		return errStatus
	}
	req.Host = f.node.host
	client := http.Client{
		Transport:     funnelProbeTransport,
		Timeout:       3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("protected loopback backend unavailable")
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") !=
		`Bearer resource_metadata="https://`+f.node.host+owneroauth.ResourceMetadataPath+`"` {
		return errors.New("protected loopback backend did not return the expected bearer challenge")
	}

	// A CLI error can arrive after tailscaled persisted the background route.
	defer func() { err = errors.Join(err, f.remove()) }()
	_, err = f.command(active, []byte("Available on the internet:\n"), "funnel", "--bg", funnelPortFlag, f.target)
	if err != nil {
		return err
	}

	// Once the write succeeds, finish its bounded readback even if stop arrives.
	sc, err := f.config(context.WithoutCancel(active))
	if err != nil || sc.snapshot() != f.published {
		return errors.New("published Funnel route could not be verified")
	}

	<-active.Done()
	return nil
}
