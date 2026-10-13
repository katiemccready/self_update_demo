//go:build !windows

package launcher

import (
	"log/slog"
	"os"
	"syscall"
)

var StopSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP}

func forward(p *os.Process, sig os.Signal, log *slog.Logger) {
	if sig == syscall.SIGINT {
		return
	}
	if err := p.Signal(sig); err != nil {
		log.Debug("could not forward signal", "signal", sig, "err", err)
	}
}

func requestStop(p *os.Process, log *slog.Logger) {
	if err := p.Signal(syscall.SIGTERM); err != nil {
		log.Debug("could not ask the program to stop", "err", err)
	}
}
