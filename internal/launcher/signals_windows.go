//go:build windows

package launcher

import (
	"log/slog"
	"os"
)

var StopSignals = []os.Signal{os.Interrupt}

func forward(*os.Process, os.Signal, *slog.Logger) {}

func requestStop(*os.Process, *slog.Logger) {}
