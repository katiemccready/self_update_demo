package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"

	"selfupdate/internal/launcher"
	"selfupdate/internal/update/install"
)

func main() { os.Exit(run()) }

func run() int {
	self, err := os.Executable()
	if err == nil {
		self, err = filepath.EvalSymlinks(self)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "launcher: cannot locate itself:", err)
		return 1
	}
	if err := launcher.KillTreeOnExit(); err != nil {
		fmt.Fprintln(os.Stderr, "launcher: cannot group the program with the launcher:", err)
	}
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, launcher.StopSignals...)
	return launcher.Run(launcher.Config{
		Root:    install.Root{Dir: filepath.Dir(self)},
		Args:    os.Args[1:],
		Signals: signals,
	})
}
