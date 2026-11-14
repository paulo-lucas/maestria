//go:build windows

package cli

import (
	"os"
	"syscall"
	"unsafe"
)

const enableEchoInput = 0x0004

var (
	kernel32       = syscall.NewLazyDLL("kernel32.dll")
	getConsoleMode = kernel32.NewProc("GetConsoleMode")
	setConsoleMode = kernel32.NewProc("SetConsoleMode")
)

// setEcho turns console echo on or off for stdin and returns a restore function.
// It returns ok=false when stdin is not a console.
func setEcho(on bool) (restore func(), ok bool) {
	h := os.Stdin.Fd()
	var mode uint32
	if r, _, _ := getConsoleMode.Call(h, uintptr(unsafe.Pointer(&mode))); r == 0 {
		return func() {}, false
	}
	next := mode &^ enableEchoInput
	if on {
		next = mode | enableEchoInput
	}
	if r, _, _ := setConsoleMode.Call(h, uintptr(next)); r == 0 {
		return func() {}, false
	}
	return func() { _, _, _ = setConsoleMode.Call(h, uintptr(mode)) }, true
}
