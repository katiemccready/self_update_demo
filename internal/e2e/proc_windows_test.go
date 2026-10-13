//go:build windows

package e2e

import (
	"os/exec"
	"strconv"
)

func ownProcessGroup(*exec.Cmd) {}

func killTree(cmd *exec.Cmd) {
	_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
}
