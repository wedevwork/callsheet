//go:build darwin

package function

import "golang.org/x/sys/unix"

// parentPID reads pid's parent through sysctl (0 when unavailable).
func parentPID(pid int) int {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0
	}
	return int(kp.Eproc.Ppid)
}
