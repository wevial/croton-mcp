package mcpserver

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/forwardpolicy"
	bolt "go.etcd.io/bbolt"
)

// The quota-only checksum prefixes timestamps with their byte length.
func seedPreviousReservation(t *testing.T, dir string) {
	t.Helper()
	db, err := bolt.Open(filepath.Join(dir, "quota.db"), 0600, nil)
	requireForwardSetup(t, err)

	err = db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("reservations-v2"))
		key := sha256.Sum256([]byte(forwardLogin))
		stamp := binary.BigEndian.AppendUint64(nil, uint64(time.Now().UnixNano()))
		if err := b.Put(key[:], stamp); err != nil {
			return err
		}

		record := append(key[:], byte(len(stamp)))
		sum := sha256.Sum256(append(record, stamp...))
		return b.Put([]byte("checksum"), sum[:])
	})
	requireForwardSetup(t, db.Close())
	requireForwardSetup(t, err)
}

func TestForwardRememberedPolicy(t *testing.T) {
	t.Run("Gate", func(t *testing.T) {
		f := plainForwardFixture(t)
		p := newSMTPPeer(t, "", "")
		p.config.RememberedRecipients = []string{"reviewer@RECIPIENT.test"}
		seedPreviousReservation(t, p.config.StateDir)

		r := f.prepare(t, false)
		bad := r
		bad.Preview.Body += "!"
		requireToolError(t, f.store.send(t.Context(), p.config, forwardRequest(t, f, bad), nil), errInvalidArgument)
		req := forwardRequest(t, f, r)
		req.Params.Arguments = append(req.Params.Arguments[:len(req.Params.Arguments)-1], `,"dailyLimit":100}`...)
		requireToolError(t, f.store.send(t.Context(), p.config, req, nil), errInvalidArgument)
		if p.connections.Load() != 0 {
			t.Fatal("invalid preview opened SMTP")
		}
		req = forwardRequest(t, f, r)
		requireSendOutcome(t, f.store.send(t.Context(), p.config, req, nil), "accepted")
		requireToolError(t, f.store.send(t.Context(), p.config, req, nil), errUnavailable)

		for _, lookalike := range []string{"Reviewer@recipient.test", "reviewer+tag@recipient.test", "reviewer@recipient.test.suffix.test"} {
			args := f.arguments(false)
			args["to"] = lookalike
			var other forwardResult
			callJSON(t, f.client, forwardTool, args, &other)
			confirmation(t, f, p.config, other)
		}

		r = f.prepare(t, false)
		requireSendOutcome(t, f.store.send(t.Context(), p.config, forwardRequest(t, f, r), nil), "accepted")
		r = f.prepare(t, false)
		for _, limit := range []int{3, 2} {
			requireForwardSetup(t, forwardpolicy.SetLimit(p.config, forwardLogin, limit))
			requireToolError(t, f.store.send(t.Context(), p.config, forwardRequest(t, f, r), nil), "quota_reached")
		}

		requireForwardSetup(t, forwardpolicy.SetLimit(p.config, forwardLogin, 4))
		// The owner can forget the recipient while SubmitForward rechecks the login.
		pending, confirmed, result := f.store.approval(p.config, forwardRequest(t, f, r))
		if result != nil || confirmed != nil || pending == nil {
			t.Fatal("remembered recipient did not pass the form gate")
		}
		requireForwardSetup(t, forwardpolicy.Forget(p.config, forwardLogin, forwardTo))
		if err := f.store.reserve(t.Context(), p.config, pending, false); err == nil {
			t.Fatal("admitted a stale remembered-recipient decision")
		}
		confirmation(t, f, p.config, r)
		if p.connections.Load() != 2 {
			t.Fatal("a refused or unconfirmed forward reached SMTP")
		}
	})

	t.Run("Learn", func(t *testing.T) {
		for _, tc := range []struct {
			name, fault, want string
			learn             bool
			connections       int32
		}{
			{"accepted", "", "accepted", true, 2},
			{"declined", "", "", false, 0},
			{"refused", "rcpt refused", "not_submitted", false, 1},
			{"unknown", "lost final reply", "unknown", false, 1},
			{"disabled", "", "accepted", false, 1},
			{"full", "", "accepted", false, 1},
			{"forget before confirmation", "", "accepted", false, 1},
			{"forget during SMTP", holdFinalReply, "accepted", false, 1},
			{"unrelated forget", holdFinalReply, "accepted", true, 2},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f := plainForwardFixture(t)
				p := newSMTPPeer(t, "", tc.fault)
				if tc.name == "disabled" {
					p.config.AutoRemember = new(false)
				}
				if tc.name == "full" {
					for i := range bridge.MaxRememberedRecipients {
						p.config.RememberedRecipients = append(p.config.RememberedRecipients, fmt.Sprintf("full%d@recipient.test", i))
					}
				}
				req := confirmation(t, f, p.config, f.prepare(t, false))
				if tc.name == "forget before confirmation" {
					requireForwardSetup(t, forwardpolicy.Forget(p.config, forwardLogin, forwardTo))
				}

				switch {
				case tc.name == "declined":
					req.Params.InputResponses[forwardConfirmation] = &mcp.ElicitResult{Action: "decline"}
					requirePreflightRefusal(t, f, p.config, req)
				case p.finalReply != nil:
					result := make(chan *mcp.CallToolResult, 1)
					go func() { result <- f.store.send(t.Context(), p.config, req, nil) }()
					select {
					case <-p.deliveries:
					case res := <-result:
						t.Fatalf("send finished before DATA, error=%t", res.IsError)
					}
					recipient := forwardTo
					if tc.name == "unrelated forget" {
						recipient = "other@recipient.test"
					}
					err := forwardpolicy.Forget(p.config, forwardLogin, recipient)
					close(p.finalReply)
					requireForwardSetup(t, err)
					requireSendOutcome(t, <-result, tc.want)
				default:
					requireSendOutcome(t, f.store.send(t.Context(), p.config, req, nil), tc.want)
				}

				// A fresh stdio session and adapter share only the private database.
				restarted := plainForwardFixture(t)
				next := restarted.prepare(t, false)
				if tc.learn {
					requireSendOutcome(t, restarted.store.send(t.Context(), p.config, forwardRequest(t, restarted, next), nil), "accepted")
				} else {
					confirmation(t, restarted, p.config, next)
				}
				policy, err := forwardpolicy.Inspect(p.config, forwardLogin)
				requireForwardSetup(t, err)
				if policy.Remembers("trap@sender.test") || policy.Remembers("source@sender.test") {
					t.Fatal("learned an address from message content")
				}
				for _, address := range p.config.RememberedRecipients {
					if !policy.Remembers(address) {
						t.Fatal("evicted a remembered recipient")
					}
				}
				if p.connections.Load() != tc.connections {
					t.Fatal("unexpected SMTP retry or bypass")
				}
			})
		}
	})
}
