//go:build !windows

package cli

import (
	"os"
	"os/exec"
)

// setEcho turns terminal echo on or off for stdin and returns a restore function.
// It returns ok=false when stdin is not a terminal (a pipe or a file), where nothing is echoed anyway.
func setEcho(on bool) (restore func(), ok bool) {
	st, err := os.Stdin.Stat()
	if err != nil || st.Mode()&os.ModeCharDevice == 0 {
		return func() {}, false
	}
	arg := "-echo"
	back := "echo"
	if on {
		arg, back = "echo", "-echo"
	}
	run := func(a string) error {
		cmd := exec.Command("stty", a)
		cmd.Stdin = os.Stdin
		return cmd.Run()
	}
	if run(arg) != nil {
		return func() {}, false
	}
	return func() { _ = run(back) }, true
}
