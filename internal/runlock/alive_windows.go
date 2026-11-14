//go:build windows

package runlock

import "os"

// alive reports whether a process with this pid exists. On Windows FindProcess opens the process
// and fails when it is gone.
func alive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}
