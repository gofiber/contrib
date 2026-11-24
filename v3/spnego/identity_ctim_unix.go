//go:build unix && !darwin && !freebsd && !netbsd

package spnego

import "syscall"

func changeTime(stat *syscall.Stat_t) uint64 {
	return uint64(stat.Ctim.Sec)*1e9 + uint64(stat.Ctim.Nsec)
}
