//go:build linux || darwin

package server

import (
	"golang.org/x/sys/unix"
	"math"
	"os"
)

// Leave room for all eight slots, multipart framing and database writes. This
// is admission control, not a substitute for a dedicated temporary volume.
func uploadSpaceAvailable(path string, maxBytes int64) bool {
	if err := os.MkdirAll(path, 0o750); err != nil {
		return false
	}
	if maxBytes > (math.MaxInt64-(64<<20))/16 {
		return false
	}
	required := uint64(64<<20) + uint64(maxBytes)*16
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return false
	}
	if stat.Bsize <= 0 {
		return false
	}
	return stat.Bavail >= required/uint64(stat.Bsize)+1
}
