package process

import "golang.org/x/sys/unix"

// sstop is SSTOP from <sys/proc.h>.
const sstop = 4

func stopped(pid int) bool {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	return err == nil && info.Proc.P_stat == sstop
}
