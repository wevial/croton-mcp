//go:build !linux && !darwin && !freebsd

package bridge

import "os/exec"

const credentialProcessTreeKillSupported = false

func configureCredentialProcess(_ *exec.Cmd) {}
