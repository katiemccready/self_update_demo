//go:build windows

package update

import (
	"os/exec"

	"selfupdate/internal/winjob"
)

func isolateProcessGroup(cmd *exec.Cmd) (started, sweep func()) {
	job, err := winjob.New()
	if err != nil {
		return func() {}, func() {}
	}
	return func() { _ = job.AssignPID(cmd.Process.Pid) }, func() { _ = job.Close() }
}
