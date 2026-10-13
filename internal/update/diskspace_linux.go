//go:build linux

package update

import "syscall"

func freeBytes(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	unit := uint64(st.Frsize)
	if unit == 0 {
		unit = uint64(st.Bsize)
	}
	return uint64(st.Bavail) * unit, nil
}
