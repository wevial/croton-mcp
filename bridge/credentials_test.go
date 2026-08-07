package bridge_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wevial/croton-mcp/bridge"
)

const (
	credentialCommandTestTimeout       = 5 * time.Second
	credentialDescendantStartTimeout   = 2 * time.Second
	credentialDescendantCommandTimeout = 3 * time.Second
	credentialDescendantMaximumWait    = 4 * time.Second
)

func TestLoadCredentialsAcceptsExactlyOneCredentialObject(t *testing.T) {
	credentials, err := bridge.LoadCredentials(context.Background(), credentialHelper(t, "valid"), credentialCommandTestTimeout)
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	if got, want := string(credentials.Username), "fixture-user"; got != want {
		t.Fatalf("username = %q, want %q", got, want)
	}
	if got, want := string(credentials.Password), "fixture-password"; got != want {
		t.Fatalf("password = %q, want %q", got, want)
	}

	for _, mode := range []string{"empty", "malformed", "invalid-utf8", "trailing", "duplicate", "case-variant"} {
		t.Run(mode, func(t *testing.T) {
			_, err := bridge.LoadCredentials(context.Background(), credentialHelper(t, mode), credentialCommandTestTimeout)
			if bridge.CodeOf(err) != bridge.CodeCredentialOutput {
				t.Fatalf("LoadCredentials(%s) error = %v, want %q", mode, err, bridge.CodeCredentialOutput)
			}
		})
	}
}

func TestLoadCredentialsFailsClosedWithoutLeakingOutput(t *testing.T) {
	const canary = "fixture-secret-must-not-appear"

	_, err := bridge.LoadCredentials(context.Background(), credentialHelper(t, "secret-malformed"), credentialCommandTestTimeout)
	if bridge.CodeOf(err) != bridge.CodeCredentialOutput {
		t.Fatalf("malformed credential error = %v, want %q", err, bridge.CodeCredentialOutput)
	}
	if strings.Contains(fmt.Sprint(err), canary) {
		t.Fatalf("credential error leaked command output: %v", err)
	}

	_, err = bridge.LoadCredentials(context.Background(), credentialHelper(t, "flood"), credentialCommandTestTimeout)
	if bridge.CodeOf(err) != bridge.CodeCredentialOverflow {
		t.Fatalf("flooding credential error = %v, want %q", err, bridge.CodeCredentialOverflow)
	}

	_, err = bridge.LoadCredentials(context.Background(), credentialHelper(t, "hang"), 20*time.Millisecond)
	if bridge.CodeOf(err) != bridge.CodeCredentialTimeout {
		t.Fatalf("hanging credential error = %v, want %q", err, bridge.CodeCredentialTimeout)
	}

	if _, err = bridge.LoadCredentials(context.Background(), []string{"credential-helper"}, credentialCommandTestTimeout); bridge.CodeOf(err) != bridge.CodeInvalidConfig {
		t.Fatalf("relative executable error = %v, want %q", err, bridge.CodeInvalidConfig)
	}
	credentials, err := bridge.LoadCredentials(context.Background(), credentialHelper(t, "valid"), credentialCommandTestTimeout)
	if err != nil {
		t.Fatalf("absolute command error = %v", err)
	}
	if got, want := string(credentials.Username), "fixture-user"; got != want {
		t.Fatalf("absolute command username = %q, want %q", got, want)
	}
}

func TestLoadCredentialsDoesNotInheritUnallowlistedEnvironment(t *testing.T) {
	t.Setenv("CROTON_UNRELATED_SECRET", "fixture-parent-only-value")

	credentials, err := bridge.LoadCredentials(context.Background(), credentialHelper(t, "environment"), credentialCommandTestTimeout)
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	if got, want := string(credentials.Password), "absent"; got != want {
		t.Fatalf("unallowlisted environment reached credential command: password = %q, want %q", got, want)
	}
}

func TestLoadCredentialsBoundsInheritedStdoutPipe(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("process-tree termination uses Linux process groups")
	}

	sentinel := t.TempDir() + "/descendant-survived"
	startedFile := t.TempDir() + "/descendant-started"
	started := time.Now()
	result := make(chan error, 1)
	go func() {
		_, err := bridge.LoadCredentials(context.Background(), credentialHelper(t, "timeout-descendant", sentinel, startedFile), credentialDescendantCommandTimeout)
		result <- err
	}()
	waitForFile(t, startedFile, credentialDescendantStartTimeout)

	var err error
	select {
	case err = <-result:
	case <-time.After(credentialDescendantMaximumWait):
		t.Fatal("timed out waiting for bounded credential command")
	}
	if bridge.CodeOf(err) != bridge.CodeCredentialTimeout {
		t.Fatalf("descendant credential error = %v, want %q", err, bridge.CodeCredentialTimeout)
	}
	if elapsed := time.Since(started); elapsed > credentialDescendantMaximumWait {
		t.Fatalf("descendant credential command waited %s for inherited stdout", elapsed)
	}

	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("timed-out credential descendant remained alive: %v", err)
	}
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()

	deadline := time.After(timeout)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	for {
		if _, err := os.Stat(path); err == nil {
			return
		}

		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", path)
		case <-ticker.C:
		}
	}
}

func credentialHelper(t *testing.T, mode string, arguments ...string) []string {
	t.Helper()

	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}
	command := []string{executable, "-test.run=TestCredentialHelperProcess", "--", mode}
	return append(command, arguments...)
}

func TestCredentialHelperProcess(t *testing.T) {
	if !strings.Contains(strings.Join(os.Args, " "), "TestCredentialHelperProcess") {
		return
	}
	mode, arguments := helperMode(os.Args)
	if mode == "" {
		return
	}

	switch mode {
	case "valid":
		fmt.Fprint(os.Stdout, `{"username":"fixture-user","password":"fixture-password"}`)
	case "empty":
	case "malformed":
		fmt.Fprint(os.Stdout, `{"username":`)
	case "invalid-utf8":
		_, _ = os.Stdout.Write([]byte{'{', '"', 'u', 's', 'e', 'r', 'n', 'a', 'm', 'e', '"', ':', '"', 0xff, '"', ',', '"', 'p', 'a', 's', 's', 'w', 'o', 'r', 'd', '"', ':', '"', 'x', '"', '}'})
	case "secret-malformed":
		fmt.Fprint(os.Stdout, `{"username":"fixture-secret-must-not-appear"}`)
	case "trailing":
		fmt.Fprint(os.Stdout, `{"username":"fixture-user","password":"fixture-password"} {}`)
	case "duplicate":
		fmt.Fprint(os.Stdout, `{"username":"fixture-user","username":"other-user","password":"fixture-password"}`)
	case "case-variant":
		fmt.Fprint(os.Stdout, `{"Username":"fixture-user","password":"fixture-password"}`)
	case "flood":
		fmt.Fprint(os.Stdout, strings.Repeat("x", 64*1024+1))
	case "hang":
		time.Sleep(time.Hour)
	case "timeout-descendant":
		child := exec.Command(os.Args[0], "-test.run=TestCredentialHelperProcess", "--", "descendant-child", arguments[0], arguments[1])
		child.Stdout = os.Stdout
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		time.Sleep(time.Hour)
	case "descendant-child":
		_ = os.WriteFile(arguments[1], []byte("started"), 0o600)
		time.Sleep(credentialDescendantCommandTimeout + time.Second)
		_ = os.WriteFile(arguments[0], []byte("survived"), 0o600)
	case "environment":
		if os.Getenv("CROTON_UNRELATED_SECRET") != "" {
			fmt.Fprint(os.Stdout, `{"username":"fixture-user","password":"present"}`)
			break
		}
		fmt.Fprint(os.Stdout, `{"username":"fixture-user","password":"absent"}`)
	default:
		os.Exit(2)
	}
	os.Exit(0)
}

func helperMode(values []string) (string, []string) {
	for index, value := range values {
		if value == "--" && index+1 < len(values) {
			return values[index+1], values[index+2:]
		}
	}

	return "", nil
}
