//go:build windows

package main

import (
	"errors"
	"log"
	"syscall"
	"time"
	"unsafe"

	hid "github.com/sstallion/go-hid"

	"mutastic/internal/daemon"
)

const yetiVID = 0x046D

var yetiPIDs = []uint16{0x0AAF, 0x0AD1, 0x0AC6}

var hidReady = false

// openYetiX finds the Yeti X vendor control interface: the HID collection
// with Usage == 1 (per docs/yeti-x-hid-protocol.md; UsagePage is logged but
// deliberately NOT filtered on, mirroring the reference implementation).
// logger is the daemon's MultiWriter logger, so the enumeration lines land
// in mutastic.log as well as on stderr (Task 8 consumes them from the file).
func openYetiX(logger *log.Logger) (daemon.Device, error) {
	if !hidReady {
		if err := hid.Init(); err != nil {
			return nil, err
		}
		hidReady = true
	}
	var path string
	for _, pid := range yetiPIDs {
		_ = hid.Enumerate(yetiVID, pid, func(info *hid.DeviceInfo) error {
			logger.Printf("hid collection: pid=0x%04x usage_page=0x%04x usage=0x%04x path=%s",
				pid, info.UsagePage, info.Usage, info.Path)
			if path == "" && info.Usage == 1 {
				path = info.Path
			}
			return nil
		})
		if path != "" {
			break
		}
	}
	if path == "" {
		return nil, errors.New("Yeti X control interface (usage==1) not found")
	}
	dev, err := hid.OpenPath(path)
	if err != nil {
		return nil, err
	}
	return wrappedDevice{dev}, nil
}

// wrappedDevice normalizes go-hid behavior to the daemon.Device contract:
// a read timeout must surface as (0, nil), not an error.
type wrappedDevice struct {
	d *hid.Device
}

func (w wrappedDevice) Write(p []byte) (int, error) { return w.d.Write(p) }

func (w wrappedDevice) ReadWithTimeout(p []byte, t time.Duration) (int, error) {
	n, err := w.d.ReadWithTimeout(p, t)
	if err != nil && errors.Is(err, hid.ErrTimeout) {
		return 0, nil
	}
	return n, err
}

func (w wrappedDevice) Close() error { return w.d.Close() }

// hideConsoleIfOwned hides this process's console window, but only when the
// process is the console's sole owner (launched via shortcut / AHK Run —
// not from an interactive terminal, whose window we must not hide).
func hideConsoleIfOwned() {
	k32 := syscall.NewLazyDLL("kernel32.dll")
	hwnd, _, _ := k32.NewProc("GetConsoleWindow").Call()
	if hwnd == 0 {
		return
	}
	pids := make([]uint32, 4)
	n, _, _ := k32.NewProc("GetConsoleProcessList").Call(
		uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	if n == 1 {
		const swHide = 0
		syscall.NewLazyDLL("user32.dll").NewProc("ShowWindow").Call(hwnd, swHide)
	}
}
