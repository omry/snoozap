//go:build windows

package main

import (
	"os"
	"syscall"
	"unsafe"

	"github.com/omry/snoozap/internal/desktop"
)

func main() {
	if desktop.Run() != nil {
		message, _ := syscall.UTF16PtrFromString("SnooZap could not complete startup or cleanup. Sleep protection is not confirmed. Use the console status command to check an existing runtime.")
		title, _ := syscall.UTF16PtrFromString("SnooZap")
		syscall.NewLazyDLL("user32.dll").NewProc("MessageBoxW").Call(0, uintptr(unsafe.Pointer(message)), uintptr(unsafe.Pointer(title)), 0x10)
		os.Exit(1)
	}
}
