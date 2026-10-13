//go:build darwin || freebsd

package update

import "syscall"

func freeBytes(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	avail := int64(st.Bavail)
	if avail < 0 {
		return 0, nil
	}
	return uint64(avail) * uint64(st.Bsize), nil
}
