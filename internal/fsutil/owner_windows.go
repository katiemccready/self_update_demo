//go:build windows

package fsutil

import "os"

func OwnedByCurrentUser(os.FileInfo) bool { return true }
