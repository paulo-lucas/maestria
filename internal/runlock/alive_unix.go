//go:build !windows

package runlock

import (
	"errors"
	"os"
	"syscall"
)

// alive reports whether a process with this pid exists (signal 0 checks without sending).
func alive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
