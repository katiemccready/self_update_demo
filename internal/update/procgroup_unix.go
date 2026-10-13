//go:build !windows

package update

import (
	"os"
	"os/exec"
	"syscall"
)

func isolateProcessGroup(cmd *exec.Cmd) (started, sweep func()) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Signal(os.Kill)
		}
		return nil
	}
	return func() {}, func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}
}
