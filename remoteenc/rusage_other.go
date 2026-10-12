//go:build !unix

package remoteenc

import "os"

// MaxRSSKB is not reported on this platform.
func MaxRSSKB(*os.ProcessState) int64 { return 0 }
