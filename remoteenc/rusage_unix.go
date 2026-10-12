//go:build unix

package remoteenc

import (
	"os"
	"runtime"
	"syscall"
)

// MaxRSSKB is a finished process's peak resident memory in KB, or 0 when the
// platform doesn't report it. Linux reports ru_maxrss in KB, macOS in bytes.
func MaxRSSKB(ps *os.ProcessState) int64 {
	if ps == nil {
		return 0
	}
	ru, ok := ps.SysUsage().(*syscall.Rusage)
	if !ok {
		return 0
	}
	if runtime.GOOS == "darwin" {
		return int64(ru.Maxrss) / 1024
	}
	return int64(ru.Maxrss)
}
