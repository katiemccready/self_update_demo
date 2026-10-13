//go:build windows

package update

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

func TestSharingViolationCountsAsBusy(t *testing.T) {
	for _, e := range []syscall.Errno{errSharingViolation, errLockViolation, errAccessDenied} {
		wrapped := fmt.Errorf("run --version: %w", &exec.Error{Name: "x", Err: &os.PathError{Op: "fork/exec", Path: "x", Err: e}})
		if !isTextBusy(wrapped) {
			t.Errorf("errno %d is not treated as busy", e)
		}
	}
	if isTextBusy(syscall.Errno(2)) {
		t.Error("file-not-found must not be treated as busy")
	}
}
