//go:build darwin || freebsd

package bridge

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

const credentialProcessTreeKillSupported = true

func configureCredentialProcess(process *exec.Cmd) {
	process.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	process.Cancel = func() error {
		if process.Process == nil {
			return os.ErrProcessDone
		}

		err := syscall.Kill(-process.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}

		return err
	}
}
