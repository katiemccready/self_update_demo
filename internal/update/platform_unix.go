//go:build !windows

package update

import (
	"errors"
	"syscall"
	"time"
)

const (
	textBusyAttempts = 6
	textBusyDelay    = 100 * time.Millisecond
)

func isTextBusy(err error) bool { return errors.Is(err, syscall.ETXTBSY) }
