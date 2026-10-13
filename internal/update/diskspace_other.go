//go:build !windows && !linux && !darwin && !freebsd

package update

import "errors"

func freeBytes(string) (uint64, error) {
	return 0, errors.New("free space query not supported on this platform")
}
