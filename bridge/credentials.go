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
	"time"
)

const (
	credentialOutputLimit = 64 * 1024
	credentialWaitDelay   = 100 * time.Millisecond
)

// Credentials holds Bridge-generated IMAP credentials as mutable bytes.
type Credentials struct {
	Username []byte
	Password []byte
}

// Zero overwrites credential bytes when the caller has finished using them.
// Go's immutable strings and runtime-managed copies cannot be reliably erased.
func (credentials *Credentials) Zero() {
	zeroBytes(credentials.Username)
	zeroBytes(credentials.Password)
}

// LoadCredentials executes an operator-configured absolute argv without an implicit shell
// and parses one credential JSON object. The command is a privileged configuration
// capability: callers must not source it or its arguments from untrusted input.
func LoadCredentials(parent context.Context, command []string, timeout time.Duration) (Credentials, error) {
	if !isSafeCredentialCommand(command) || timeout <= 0 {
		return Credentials{}, errorCode(CodeInvalidConfig)
	}

	contextWithTimeout, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	output := &limitedBuffer{limit: credentialOutputLimit}
	process := exec.CommandContext(contextWithTimeout, command[0], command[1:]...)
	configureCredentialProcess(process)
	process.Stdin = bytes.NewReader(nil)
	process.Stdout = output
	process.Stderr = io.Discard
	process.Env = credentialEnvironment()
	// WaitDelay bounds inherited stdout pipes after direct-child cancellation,
	// including on platforms without process-group termination.
	process.WaitDelay = credentialWaitDelay

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
	return len(command) > 0 && filepath.IsAbs(command[0])
}

func parseCredentials(output []byte) (Credentials, error) {
	decoder := json.NewDecoder(bytes.NewReader(output))
	start, err := decoder.Token()
	if err != nil {
		return Credentials{}, errorCode(CodeCredentialOutput)
	}
	if delimiter, ok := start.(json.Delim); !ok || delimiter != '{' {
		return Credentials{}, errorCode(CodeCredentialOutput)
	}

	values := make(map[string]string, 2)
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok || (name != "username" && name != "password") {
			return Credentials{}, errorCode(CodeCredentialOutput)
		}
		if _, exists := values[name]; exists {
			return Credentials{}, errorCode(CodeCredentialOutput)
		}

		var value string
		if err := decoder.Decode(&value); err != nil {
			return Credentials{}, errorCode(CodeCredentialOutput)
		}
		values[name] = value
	}

	end, err := decoder.Token()
	if err != nil {
		return Credentials{}, errorCode(CodeCredentialOutput)
	}
	if delimiter, ok := end.(json.Delim); !ok || delimiter != '}' {
		return Credentials{}, errorCode(CodeCredentialOutput)
	}
	if values["username"] == "" || values["password"] == "" {
		return Credentials{}, errorCode(CodeCredentialOutput)
	}
	if err := ensureOnlyWhitespace(decoder); err != nil {
		return Credentials{}, errorCode(CodeCredentialOutput)
	}

	return Credentials{Username: []byte(values["username"]), Password: []byte(values["password"])}, nil
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
