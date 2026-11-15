//go:build windows

package runlock

import (
	"errors"

	"golang.org/x/sys/windows"
)

// stillActive is the exit code GetExitCodeProcess reports for a process that has not exited.
const stillActive = 259

// alive reports whether a process with this pid is running. OpenProcess alone is not enough on
// Windows: it also succeeds for a process that already exited while any handle to it is still
// open, so the exit code is checked too.
func alive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		// Access denied means the process exists but belongs to someone else; any other error
		// (typically ERROR_INVALID_PARAMETER) means there is no such process.
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true // cannot tell: assume alive, the heartbeat staleness check still applies
	}
	return code == stillActive
}
