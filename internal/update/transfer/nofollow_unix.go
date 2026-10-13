//go:build !windows

package transfer

import "syscall"

const openNoFollow = syscall.O_NOFOLLOW
