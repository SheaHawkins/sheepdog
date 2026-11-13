//go:build !windows

package run

import (
	"errors"
	"syscall"
)

// alive reports whether a process exists. EPERM means it exists but belongs
// to someone else.
func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
