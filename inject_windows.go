//go:build windows

package main

import (
	"fmt"
	"syscall"
	"unsafe"

	"mutastic/internal/daemon"
)

// Win32 constants for SendInput.
const (
	inputKeyboard  = 1      // INPUT.type: keyboard event
	keyeventfKeyup = 0x0002 // KEYBDINPUT.dwFlags: key release
	vkF24          = 0x87   // VK_F24 — free on this machine (pedal firmware uses F13/F14/F15)
)

// input mirrors the Win32 INPUT struct carrying a KEYBDINPUT on 64-bit
// Windows: DWORD type at offset 0, 4 bytes padding (the union is 8-byte
// aligned), KEYBDINPUT at offset 8, then trailing padding out to the
// union's largest member (MOUSEINPUT, 32 bytes). Total: 40 bytes. Field
// offsets verified against mingw-w64 <windows.h> by a compile-time
// _Static_assert probe (2026-08-09 load-bearing sweep).
type input struct {
	inputType   uint32  // INPUT.type
	_           uint32  // padding before the 8-aligned union
	wVk         uint16  // KEYBDINPUT.wVk: virtual-key code
	wScan       uint16  // KEYBDINPUT.wScan: hardware scan code (unused)
	dwFlags     uint32  // KEYBDINPUT.dwFlags: 0 = down, KEYEVENTF_KEYUP = up
	time        uint32  // KEYBDINPUT.time: 0 = system-supplied timestamp
	_           uint32  // padding: dwExtraInfo is 8-byte aligned
	dwExtraInfo uintptr // KEYBDINPUT.dwExtraInfo
	_           [8]byte // pad the union out to MOUSEINPUT's 32 bytes
}

// Compile-time size guard: each line fails to compile (negative untyped
// constant) if input drifts from the 40-byte x64 INPUT layout in either
// direction.
const (
	_ = uint64(unsafe.Sizeof(input{}) - 40)
	_ = uint64(40 - unsafe.Sizeof(input{}))
)

var procSendInput = syscall.NewLazyDLL("user32.dll").NewProc("SendInput")

// f24Injector delivers a synthetic F24 press (down then up) to the active
// desktop via user32 SendInput, firing the AHK script's *F24:: meeting-app
// sweep. SendInput succeeds whether or not the AHK script is running —
// with no listener the keystroke lands nowhere, which is harmless by
// design. It also reports success when UIPI silently discards the input
// (elevated foreground window) — documented, undetectable; see the
// README troubleshooting entry.
type f24Injector struct{}

func (f24Injector) Inject() error {
	events := [2]input{
		{inputType: inputKeyboard, wVk: vkF24},
		{inputType: inputKeyboard, wVk: vkF24, dwFlags: keyeventfKeyup},
	}
	n, _, callErr := procSendInput.Call(
		uintptr(len(events)),
		uintptr(unsafe.Pointer(&events[0])),
		unsafe.Sizeof(events[0]),
	)
	if n != uintptr(len(events)) {
		return fmt.Errorf("SendInput inserted %d of %d events: %v", n, len(events), callErr)
	}
	return nil
}

// newKeyInjector returns the platform key injector (see inject_other.go
// for the non-Windows counterpart).
func newKeyInjector() daemon.KeyInjector { return f24Injector{} }
