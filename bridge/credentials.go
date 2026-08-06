package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const credentialOutputLimit = 64 * 1024

// Credentials holds Bridge-generated IMAP credentials as mutable bytes.
type Credentials struct {
	Username []byte
	Password []byte
}

// Zero overwrites credential bytes when the caller has finished using them.
func (credentials *Credentials) Zero() {
	zeroBytes(credentials.Username)
	zeroBytes(credentials.Password)
}

// LoadCredentials executes an absolute argv-only command and parses one credential JSON object.
func LoadCredentials(parent context.Context, command []string, timeout time.Duration) (Credentials, error) {
	if !isSafeCredentialCommand(command) || timeout <= 0 {
		return Credentials{}, errorCode(CodeInvalidConfig)
	}

	contextWithTimeout, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	output := &limitedBuffer{limit: credentialOutputLimit}
	process := exec.CommandContext(contextWithTimeout, command[0], command[1:]...)
	process.Stdin = bytes.NewReader(nil)
	process.Stdout = output
	process.Stderr = io.Discard
	process.Env = credentialEnvironment()

	err := process.Run()
	if output.overflow {
		zeroBytes(output.bytes)
		return Credentials{}, errorCode(CodeCredentialOverflow)
	}
	if errors.Is(contextWithTimeout.Err(), context.DeadlineExceeded) {
		zeroBytes(output.bytes)
		return Credentials{}, errorCode(CodeCredentialTimeout)
	}
	if err != nil {
		zeroBytes(output.bytes)
		return Credentials{}, errorCode(CodeCredentialCommand)
	}

	defer zeroBytes(output.bytes)
	return parseCredentials(output.bytes)
}

type limitedBuffer struct {
	bytes    []byte
	limit    int
	overflow bool
}

func (buffer *limitedBuffer) Write(input []byte) (int, error) {
	if len(input) > buffer.limit-len(buffer.bytes) {
		buffer.overflow = true
		return len(input), nil
	}
	buffer.bytes = append(buffer.bytes, input...)
	return len(input), nil
}

func credentialEnvironment() []string {
	environment := []string{"PATH=/usr/bin:/bin"}
	for _, key := range []string{"HOME", "USER", "LOGNAME", "LANG", "LC_ALL"} {
		if value, ok := os.LookupEnv(key); ok {
			environment = append(environment, key+"="+value)
		}
	}
	return environment
}

func isSafeCredentialCommand(command []string) bool {
	if len(command) == 0 || !filepath.IsAbs(command[0]) {
		return false
	}

	switch strings.ToLower(filepath.Base(command[0])) {
	case "sh", "bash", "dash", "zsh", "ksh", "mksh", "fish", "csh", "tcsh", "pwsh", "powershell", "cmd", "cmd.exe":
		return false
	default:
		return true
	}
}

func parseCredentials(output []byte) (Credentials, error) {
	var value struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}

	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return Credentials{}, errorCode(CodeCredentialOutput)
	}
	if value.Username == "" || value.Password == "" {
		return Credentials{}, errorCode(CodeCredentialOutput)
	}
	if err := ensureOnlyWhitespace(decoder); err != nil {
		return Credentials{}, errorCode(CodeCredentialOutput)
	}

	return Credentials{Username: []byte(value.Username), Password: []byte(value.Password)}, nil
}

func ensureOnlyWhitespace(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("additional JSON value")
	}
	return nil
}

func zeroBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
