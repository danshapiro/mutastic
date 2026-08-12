//go:build windows

package main

import (
	"fmt"
	"syscall"
	"unsafe"
)

var shellExecuteW = syscall.NewLazyDLL("shell32.dll").NewProc("ShellExecuteW")

// openBrowser asks the Windows shell to open the URL with the user's default
// browser. ShellExecuteW also focuses an already-open tab/window when the
// browser chooses to reuse it.
func openBrowser(url string) error {
	verb, err := syscall.UTF16PtrFromString("open")
	if err != nil {
		return err
	}
	target, err := syscall.UTF16PtrFromString(url)
	if err != nil {
		return err
	}
	result, _, callErr := shellExecuteW.Call(
		0,
		uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(target)),
		0,
		0,
		1,
	)
	if result <= 32 {
		return fmt.Errorf("ShellExecuteW returned %d: %v", result, callErr)
	}
	return nil
}
