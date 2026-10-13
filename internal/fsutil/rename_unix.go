//go:build !windows

package fsutil

func isTransientRenameErr(error) bool { return false }
