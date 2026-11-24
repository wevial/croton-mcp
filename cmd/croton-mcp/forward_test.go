package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wevial/croton-mcp/internal/owneroauth"
)

func TestForwardOwnerCommands(t *testing.T) {
	tmp, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(tmp, "croton-owner-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})

	path := filepath.Join(root, "config.json")
	credentials := filepath.Join(root, "synthetic.json")
	cat, err := exec.LookPath("cat")
	if err != nil {
		t.Fatal(err)
	}
	login := func(address string) {
		t.Helper()
		data, _ := json.Marshal(map[string]string{"username": address, "password": "synthetic"})
		if err := os.WriteFile(credentials, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	forward := map[string]any{
		"enabled":              true,
		"smtpPort":             1025,
		"stateDir":             filepath.Join(root, "forward"),
		"dailyLimit":           3,
		"rememberedRecipients": []string{"seed@recipient.test"},
	}
	writeConfig := func() {
		t.Helper()
		data, err := json.Marshal(map[string]any{"forward": forward, "imap": map[string]any{
			"tls": map[string]string{"spkiSha256": strings.Repeat("1", 64)}, "credentialCommand": []string{cat, credentials},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	invoke := func(wantSuccess bool, args ...string) string {
		t.Helper()
		var out bytes.Buffer
		err := run(t.Context(), append(args, "--config", path), &out, owneroauth.Defaults())
		if (err == nil) != wantSuccess {
			t.Fatalf("%s returned %v", args[0], err)
		}
		return out.String()
	}
	limits := func(limit, count int) {
		t.Helper()
		want := fmt.Sprintf("dailyLimit=%d autoRemember=true rememberedRecipients=%d/100\n", limit, count)
		if got := invoke(true, "limits"); got != want {
			t.Errorf("limits = %q, want %q", got, want)
		}
	}

	login("owner@bridge.test")
	writeConfig()
	invoke(true, "init-forward-state")
	limits(3, 1)
	invoke(true, "forget", "seed@RECIPIENT.test")
	limits(3, 0)
	invoke(true, "allow", "new@recipient.test")
	invoke(true, "allow", "new@RECIPIENT.test")
	invoke(true, "limits", "1")

	for _, args := range [][]string{
		{"allow", "Alice <alice@recipient.test>"}, {"forget", "new@recipient.test,other@recipient.test"}, {"allow", ""},
		{"forget"}, {"limits", "0"}, {"limits", "101"}, {"limits", "x"}, {"limits", "1", "2"},
	} {
		invoke(false, args...)
	}
	limits(1, 1)

	// Start a fresh executable against the same private state and modified seeds.
	forward["dailyLimit"] = 9
	forward["rememberedRecipients"] = []string{"seed@recipient.test", "another@recipient.test"}
	writeConfig()
	var out bytes.Buffer
	cmd := serverCommand(t, path, &out)
	cmd.Args = []string{cmd.Path, "limits", "--config", path}
	if err := cmd.Run(); err != nil || out.String() != "dailyLimit=1 autoRemember=true rememberedRecipients=1/100\n" {
		t.Fatalf("restart limits = %q, %v", out.String(), err)
	}
	invoke(true, "forget", "new@recipient.test")
	limits(1, 0)

	login("second@bridge.test")
	limits(9, 2)
	invoke(true, "limits", "100")
	login("owner@bridge.test")
	limits(1, 0)

	login("third@bridge.test")
	delete(forward, "dailyLimit")
	delete(forward, "rememberedRecipients")
	writeConfig()
	limits(10, 0)
	for key, bad := range map[string][]any{
		"dailyLimit": {0, 101, 1.5, "10", nil}, "autoRemember": {"false", nil},
		"rememberedRecipients": {[]string{"x@recipient.test,y@recipient.test"}, "x@recipient.test"},
	} {
		for _, v := range bad {
			forward[key] = v
			writeConfig()
			invoke(false, "limits")
		}
		delete(forward, key)
	}
	login("full@bridge.test")
	addresses := make([]string, 100)
	for i := range addresses {
		addresses[i] = fmt.Sprintf("recipient%d@recipient.test", i)
	}
	forward["rememberedRecipients"] = append(addresses, "overflow@recipient.test")
	writeConfig()
	invoke(false, "limits")
	forward["rememberedRecipients"] = addresses
	writeConfig()
	limits(10, 100)
	invoke(false, "allow", "overflow@recipient.test")
	invoke(true, "allow", addresses[0])
	invoke(true, "forget", addresses[99])
	limits(10, 99)
}
