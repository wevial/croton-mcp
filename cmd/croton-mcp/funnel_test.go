//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wevial/croton-mcp/internal/owneroauth"
)

const funnelNodeJSON = `{"BackendState":"Running","Self":{"ID":"node.test","DNSName":"croton.tail.test.","CapMap":{
	"https":null,"funnel":[],"https://tailscale.com/cap/funnel-ports?ports=440-445,8443":null}},
	"CertDomains":["croton.tail.test"]}`

// Both status commands read the same configuration, as they do on the node.
const funnelScript = `#!/bin/sh
set -eu
cd "${0%/*}"
# Snapshot Croton's own probe result as soon as the publication command starts.
if [ "$1 ${2-}" = 'funnel --bg' ]; then
  expected="GET $4/mcp croton.tail.test 401 "'Bearer resource_metadata="https://croton.tail.test/.well-known/oauth-protected-resource/mcp"'
  if [ ! -f probe-response ] || ! IFS= read -r response < probe-response || [ "$response" != "$expected" ]; then
    printf '%s\n' 'publication started before Croton received the expected bearer challenge' >> calls
  fi
fi
printf '%s\n' "$*" >> calls
case "$1 ${2-}" in
  'funnel --bg'|'funnel --https=443')
    if [ -f version-warning ] || [ -f prompt ]; then
      printf 'Warning: client version "1.104.5-tabc" != tailscaled server version "1.104.6-tdef"\n' >&2
    fi ;;
esac
case "$*" in
  version) exec cat version ;;
  'status --json') exec cat node ;;
  'serve status --json') exec cat config ;;
  'funnel status --json')
    if [ -f inconsistent ]; then exec cat inconsistent; fi
    if [ -f published ] && [ -f fail-readback ]; then
      rm fail-readback
      exit 1
    fi
    cat config
    if [ -f published ]; then touch readback; fi ;;
  'funnel --bg --https=443 '*)
    [ "$#" = 4 ] && [ "$4" = "$(cat target)" ] || exit 2
    if [ -f prompt ]; then
      printf 'To enable Funnel, visit private.test\n' >&2
      exec sleep 30
    fi
    cp owned config
    touch published
    if [ -f fail-publish ]; then exit 1; fi
    printf 'Available on the internet:\n'
    ;;
  'funnel --https=443 off')
    if [ -f version-warning ]; then sleep 0.05; fi
    if [ ! -f keep-route ]; then cp before config; fi ;;
  *) exit 2 ;;
esac
`

type funnelFixture struct {
	dir, state, target, owned string
	args                      []string
}

func (f funnelFixture) write(t *testing.T, name, data string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(data), 0700); err != nil {
		t.Fatal(err)
	}
}

func (f funnelFixture) calls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func acceptFunnel(ctx context.Context, host, target string) error {
	return readFunnelApproval(ctx, strings.NewReader("yes\n"), io.Discard, host, target)
}

func newFunnelFixture(t *testing.T, config string) funnelFixture {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := probe.Addr().String()
	probe.Close()

	f := funnelFixture{dir: t.TempDir(), state: httpStateDir(t), target: "http://" + address}
	f.args = []string{"serve", "--public=tailscale", "--authorizer", "owner", "--config", config, "--state-dir", f.state, "--bind", address}
	f.write(t, "tailscale", funnelScript)
	f.write(t, "version", "1.104.5\n  long version: 1.104.5-fixture\n")
	f.write(t, "node", funnelNodeJSON)
	// Existing handlers and their case-sensitive mounts must survive publication and removal.
	const otherWeb = `"croton.tail.test:8443":{"Handlers":{
		"/api":{"Proxy":"http://127.0.0.1:3000","AcceptAppCaps":["example.test/cap/read"]},
		"/API":{"Text":"upper"},"/files":{"Path":"/srv/files"},"/old":{"Redirect":"/api"}}}`
	const before = `{"TCP":{"8443":{"HTTPS":true}},"AllowFunnel":null,"Web":{` + otherWeb + `}}`
	f.write(t, "config", before)
	f.write(t, "before", before)
	f.write(t, "target", f.target)
	f.owned = fmt.Sprintf(`{"TCP":{"8443":{"HTTPS":true},"443":{"HTTPS":true}},
		"AllowFunnel":{"croton.tail.test:443":true},"Web":{"croton.tail.test:443":{"Handlers":{"/":{"Proxy":%q}}},%s}}`, f.target, otherWeb)
	f.write(t, "owned", f.owned)
	t.Setenv("PATH", f.dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return f
}

var preflightCalls = []string{"version", "status --json", "serve status --json", "funnel status --json"}

func TestFunnelServePreflight(t *testing.T) {
	const occupied = " configuration occupies port 443. Move it to another port or use a dedicated node"
	const web8443 = `{"TCP":{"8443":{"HTTPS":true}},"Web":{"private.test:8443":{"Handlers":{"/":%s}}}}`
	config := writeServerConfig(t, fixtureBridge(t))
	for _, tc := range []struct{ name, file, data, diagnostic string }{
		{"old CLI", "version", "1.104.4", "1.104.5"},
		{"unreadable version", "version", "private.test", "1.104.5"},
		{"background", "config", `{"TCP":{"443":{"HTTPS":true}}}`, "Serve TCP" + occupied},
		{"foreground", "config", `{"Foreground":{"private.test":{"TCP":{"443":{"HTTP":true}}}}}`, "foreground Serve/Funnel: Serve TCP" + occupied},
		{"Web", "config", `{"Web":{"private.test:443":{"Handlers":{"/":{"Text":"private.test"}}}}}`, "Serve Web" + occupied},
		{"Funnel", "config", `{"AllowFunnel":{"private.test:443":true}}`, "AllowFunnel" + occupied},
		{"missing node ID", "node", strings.Replace(funnelNodeJSON, `"ID":"node.test",`, "", 1), "Run tailscale"},
		{"null node ID", "node", strings.Replace(funnelNodeJSON, `"node.test"`, "null", 1), "Run tailscale"},
		{"empty node ID", "node", strings.Replace(funnelNodeJSON, `"node.test"`, `""`, 1), "Run tailscale"},
		{"missing HTTPS", "node", strings.Replace(funnelNodeJSON, `"https":null,`, "", 1), "eligibility"},
		{"missing Funnel", "node", strings.Replace(funnelNodeJSON, `"funnel":[]`, `"other":[]`, 1), "eligibility"},
		{"wrong ports", "node", strings.Replace(funnelNodeJSON, "440-445", "1443", 1), "443"},
		{"invalid ports", "node", strings.Replace(funnelNodeJSON, "440-445", "443,invalid", 1), "443"},
		{"wrong certificate", "node", strings.Replace(funnelNodeJSON, `["croton.tail.test"]`, `["private.test"]`, 1), "certificate"},
		{"unsafe DNS", "node", strings.ReplaceAll(funnelNodeJSON, "croton.tail", `bad\nprivate.test`), "Run tailscale"},
		{"unreadable status", "node", "", "Run tailscale"},
		{"truncated config", "config", `{"TCP":`, "Run tailscale"},
		{"wrong shape", "config", "null", "Run tailscale"},
		{"duplicate field", "config", `{"TCP":{"443":{}},"TCP":{}}`, "Run tailscale"},
		{"ambiguous field", "config", `{"TCP":{"443":{}},"tcp":{}}`, "Run tailscale"},
		{"ambiguous handler", "config", `{"Web":{"private.test:8443":{"Handlers":{"/api":{"Text":"x","text":"y"}}}}}`, "Run tailscale"},
		{"duplicate mount", "config", `{"Web":{"private.test:8443":{"Handlers":{"/api":{"Text":"x"},"/api":{"Text":"y"}}}}}`, "Run tailscale"},
		{"targetless handler", "config", fmt.Sprintf(web8443, `{}`), "Run tailscale"},
		{"multiple handler targets", "config", fmt.Sprintf(web8443, `{"Text":"x","Redirect":"/other"}`), "Run tailscale"},
		{"invalid handler target", "config", fmt.Sprintf(web8443, `{"Proxy":true}`), "Run tailscale"},
		{"invalid port", "config", `{"AllowFunnel":{"private.test":true}}`, "Run tailscale"},
		{"inconsistent status", "inconsistent", "{}", "changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFunnelFixture(t, config)
			f.write(t, tc.file, tc.data)
			err := runCLI(t.Context(), f.args, io.Discard, owneroauth.Defaults(), func(context.Context, string, string) error {
				t.Fatal("preflight reached approval")
				return errDeclined
			})
			if err == nil || !strings.Contains(err.Error(), tc.diagnostic) || strings.Contains(err.Error(), "private.test") {
				t.Fatalf("unexpected refusal: %v", err)
			}
			calls := f.calls(t)
			if len(calls) > len(preflightCalls) || !slices.Equal(calls, preflightCalls[:len(calls)]) {
				t.Fatalf("unexpected commands %q", calls)
			}
		})
	}
}

func TestFunnelServeApproval(t *testing.T) {
	config := writeServerConfig(t, fixtureBridge(t))
	for _, tc := range []struct{ name, answer string }{
		{"no", "no\n"}, {"EOF", ""}, {"partial", "yes"}, {"cancel", ""}, {"occupied after yes", "yes\n"}, {"new origin after yes", "yes\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFunnelFixture(t, config)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			confirm := func(ctx context.Context, host, target string) error {
				var display bytes.Buffer
				var answer io.Reader = strings.NewReader(tc.answer)
				if tc.name == "cancel" {
					r, w := io.Pipe()
					defer w.Close()
					answer = r
					cancel()
				}

				err := readFunnelApproval(ctx, answer, &display, host, target)
				review, prompt, _ := strings.Cut(display.String(), "Type yes")
				if prompt == "" {
					t.Fatal("missing confirmation")
				}
				for _, want := range []string{"https://croton.tail.test", f.target, "All routes on port 443", "exclusive operator control",
					"/mcp,", "/.well-known/oauth-protected-resource/mcp,", "/.well-known/oauth-authorization-server,", "/register,",
					"/authorize,", "/token,", "/owner/pair,", "/owner/signin,", "/owner/finish\n"} {
					if !strings.Contains(review, want) {
						t.Errorf("review is missing %q", want)
					}
				}
				switch tc.name {
				case "occupied after yes":
					f.write(t, "config", `{"Foreground":{"session.test":{"TCP":{"443":{"HTTPS":true}}}}}`)
				case "new origin after yes":
					f.write(t, "node", strings.ReplaceAll(funnelNodeJSON, "croton.tail", "other.tail"))
				}
				return err
			}

			if err := runCLI(ctx, f.args, io.Discard, owneroauth.Defaults(), confirm); err == nil {
				t.Fatal("started without a current approval")
			}
			want := preflightCalls
			if tc.answer == "yes\n" {
				want = slices.Concat(preflightCalls, preflightCalls)
			}
			if calls := f.calls(t); !slices.Equal(calls, want) {
				t.Fatalf("unexpected commands %q", calls)
			}
			if files, err := os.ReadDir(f.state); err != nil || len(files) != 0 {
				t.Fatal("approval refusal changed owner state")
			}
		})
	}
}

type recordingFunnelTransport struct {
	http.RoundTripper
	responseFile string
}

func (r recordingFunnelTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.RoundTripper.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	line := fmt.Sprintf("%s %s %s %d %s\n", req.Method, req.URL, req.Host, resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	if err := os.WriteFile(r.responseFile, []byte(line), 0600); err != nil {
		resp.Body.Close()
		return nil, err
	}

	return resp, nil
}

func TestFunnelServeLifecycle(t *testing.T) {
	config := writeServerConfig(t, fixtureBridge(t))
	for _, tc := range []struct {
		name, diagnostic string
		serves, removes  bool
	}{
		{"stop", "", true, true},
		{"version-warning", "", true, true},
		{"fail-publish", "Tailscale", false, true},
		{"fail-readback", "verified", false, true},
		{"wrong readback", "verified", false, false},
		{"other mount", "ownership", true, false},
		{"different node", "ownership", true, false},
		{"empty node ID", "ownership", true, false},
		{"replaced target", "ownership", true, false},
		{"changed foreground", "ownership", true, false},
		{"unreadable at stop", "ownership", true, false},
		{"keep-route", "removal", true, true},
		{"prompt", "eligibility", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFunnelFixture(t, config)
			f.write(t, tc.name, "")
			if tc.name == "wrong readback" {
				f.write(t, "owned", `{"AllowFunnel":{"croton.tail.test:443":true}}`)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)

			transport := funnelProbeTransport
			funnelProbeTransport = recordingFunnelTransport{transport, filepath.Join(f.dir, "probe-response")}

			done := make(chan error, 1)
			go func() {
				defer close(done)
				done <- runCLI(ctx, f.args, io.Discard, owneroauth.Defaults(), acceptFunnel)
			}()
			defer func() {
				cancel()
				<-done
				funnelProbeTransport = transport
			}()

			if tc.serves {
				waitForMarker(t, filepath.Join(f.dir, "readback"))
				switch tc.name {
				case "different node":
					f.write(t, "node", strings.Replace(funnelNodeJSON, "node.test", "other.test", 1))
				case "empty node ID":
					f.write(t, "node", strings.Replace(funnelNodeJSON, `"node.test"`, `""`, 1))
				case "other mount":
					f.write(t, "config", strings.Replace(f.owned, `"Handlers":{`, `"Handlers":{"/private":{"Text":"private.test"},`, 1))
				case "replaced target":
					f.write(t, "config", strings.Replace(f.owned, f.target, "http://127.0.0.1:9000", 1))
				case "changed foreground":
					f.write(t, "config", strings.Replace(f.owned, `"Web":`, `"Foreground":{"private.test":{"TCP":{"443":{"HTTP":true}}}},"Web":`, 1))
				case "unreadable at stop":
					f.write(t, "config", "private.test")
				}
				cancel()
			}
			err := <-done
			if tc.diagnostic == "" && err != nil || tc.diagnostic != "" && (err == nil || !strings.Contains(err.Error(), tc.diagnostic)) {
				t.Fatalf("unexpected exit: %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "private.test") {
				t.Fatal("Tailscale output leaked")
			}

			want := slices.Concat(preflightCalls, preflightCalls, []string{"funnel --bg --https=443 " + f.target})
			readback := []string{"serve status --json", "funnel status --json"}
			if tc.serves || tc.name == "fail-readback" || tc.name == "wrong readback" {
				want = append(want, readback...)
			}
			want = append(want, "status --json")
			if tc.name != "different node" && tc.name != "empty node ID" {
				want = append(want, "serve status --json")
				if tc.name != "unreadable at stop" {
					want = append(want, "funnel status --json")
				}
			}
			if tc.removes {
				want = append(want, "funnel --https=443 off")
				want = append(want, readback...)
			}
			if calls := f.calls(t); !slices.Equal(calls, want) {
				t.Fatalf("commands %q, want %q", calls, want)
			}

			conn, err := net.DialTimeout("tcp", strings.TrimPrefix(f.target, "http://"), time.Second)
			if err == nil {
				conn.Close()
				t.Fatal("backend still accepts connections after exit")
			}
		})
	}
}

func TestFunnelServeBackendUnavailable(t *testing.T) {
	f := newFunnelFixture(t, writeServerConfig(t, fixtureBridge(t)))
	other, err := net.Listen("tcp", strings.TrimPrefix(f.target, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	err = runCLI(t.Context(), f.args, io.Discard, owneroauth.Defaults(), acceptFunnel)
	if err == nil || err.Error() != "HTTP listener unavailable" {
		t.Fatalf("unexpected startup result: %v", err)
	}
	if got := f.calls(t); !slices.Equal(got, slices.Concat(preflightCalls, preflightCalls)) {
		t.Fatalf("commands after bind failure: %q", got)
	}
}

func TestFunnelServeBind(t *testing.T) {
	config := writeServerConfig(t, fixtureBridge(t))
	for _, bind := range []string{"127.0.0.2:8787", "[::ffff:127.0.0.1]:8787", "[::1]:8787"} {
		t.Run(bind, func(t *testing.T) {
			f := newFunnelFixture(t, config)
			f.args[len(f.args)-1] = bind
			asked := false
			err := runCLI(t.Context(), f.args, io.Discard, owneroauth.Defaults(), func(_ context.Context, _, target string) error {
				asked = true
				if target != "http://"+bind {
					t.Errorf("approval target %q", target)
				}
				return errDeclined
			})

			if bind == "[::1]:8787" {
				if !asked || err != errDeclined {
					t.Fatalf("IPv6 loopback approval: %v", err)
				}
				return
			}
			if asked || err == nil || !strings.Contains(err.Error(), "--bind=127.0.0.1:8787") {
				t.Fatalf("incompatible bind reached approval or lacked a usable bind: %v", err)
			}
		})
	}
}
