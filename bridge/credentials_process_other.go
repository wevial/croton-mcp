//go:build !linux

package bridge

import "os/exec"

func configureCredentialProcess(_ *exec.Cmd) {}
