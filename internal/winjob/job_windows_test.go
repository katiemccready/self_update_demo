//go:build windows

package winjob

import (
	"os/exec"
	"testing"
	"time"
)

func TestClosingTheJobKillsItsProcesses(t *testing.T) {
	j, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	cmd := exec.Command("ping", "-n", "60", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := j.AssignPID(cmd.Process.Pid); err != nil {
		cmd.Process.Kill()
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	time.Sleep(200 * time.Millisecond)
	select {
	case <-exited:
		t.Fatal("process ended before the job was closed")
	default:
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		t.Fatal("closing the job did not kill its process")
	}
}
