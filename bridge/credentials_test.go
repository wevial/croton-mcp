package bridge_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wevial/croton-mcp/bridge"
)

const credentialCommandTestTimeout = 5 * time.Second

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

	for _, mode := range []string{"empty", "malformed", "trailing"} {
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
	if _, err = bridge.LoadCredentials(context.Background(), []string{"/bin/sh", "-c", "echo credential"}, credentialCommandTestTimeout); bridge.CodeOf(err) != bridge.CodeInvalidConfig {
		t.Fatalf("shell executable error = %v, want %q", err, bridge.CodeInvalidConfig)
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

func credentialHelper(t *testing.T, mode string) []string {
	t.Helper()

	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}
	return []string{executable, "-test.run=TestCredentialHelperProcess", "--", mode}
}

func TestCredentialHelperProcess(t *testing.T) {
	if !strings.Contains(strings.Join(os.Args, " "), "TestCredentialHelperProcess") || len(os.Args) < 2 {
		return
	}

	switch os.Args[len(os.Args)-1] {
	case "valid":
		fmt.Fprint(os.Stdout, `{"username":"fixture-user","password":"fixture-password"}`)
	case "empty":
	case "malformed":
		fmt.Fprint(os.Stdout, `{"username":`)
	case "secret-malformed":
		fmt.Fprint(os.Stdout, `{"username":"fixture-secret-must-not-appear"}`)
	case "trailing":
		fmt.Fprint(os.Stdout, `{"username":"fixture-user","password":"fixture-password"} {}`)
	case "flood":
		fmt.Fprint(os.Stdout, strings.Repeat("x", 64*1024+1))
	case "hang":
		time.Sleep(time.Hour)
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
