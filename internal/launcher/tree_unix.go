//go:build !windows

package launcher

func KillTreeOnExit() error { return nil }
