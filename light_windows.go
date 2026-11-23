//go:build windows

package main

import (
	"fmt"
	"strings"
	"time"

	"go.bug.st/serial"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"mutastic/internal/light"
)

// GUID_DEVINTERFACE_COMPORT identifies Windows serial port interfaces.
var comPortGUID = windows.GUID{
	Data1: 0x86e0d1e0, Data2: 0x8089, Data3: 0x11d0,
	Data4: [8]byte{0x9c, 0xe4, 0x08, 0x00, 0x3e, 0x30, 0x1f, 0x73},
}

// enumerateLightCandidates reads Windows Plug and Play metadata without
// opening a serial port. A CH340 bridge is a candidate until explicitly
// paired. The COM label and interface path both come from the same devnode.
func enumerateLightCandidates() (map[string]string, error) {
	devices, err := windows.SetupDiGetClassDevsEx(&comPortGUID, "USB", 0,
		windows.DIGCF_PRESENT|windows.DIGCF_DEVICEINTERFACE, 0, "")
	if err != nil {
		return nil, err
	}
	defer devices.Close()
	ports := make(map[string]string)
	for i := 0; ; i++ {
		device, err := devices.EnumDeviceInfo(i)
		if err == windows.ERROR_NO_MORE_ITEMS {
			return ports, nil
		}
		if err != nil {
			return nil, fmt.Errorf("enumerating USB serial devices: %w", err)
		}
		id, err := devices.DeviceInstanceID(device)
		if err != nil {
			continue
		}
		const ch340 = `USB\VID_1A86&PID_7523`
		upper := strings.ToUpper(id)
		if !strings.HasPrefix(upper, ch340+`\`) && !strings.HasPrefix(upper, ch340+"&") {
			continue
		}
		paths, err := windows.CM_Get_Device_Interface_List(id, &comPortGUID, windows.CM_GET_DEVICE_INTERFACE_LIST_PRESENT)
		if err != nil || len(paths) != 1 {
			continue
		}
		handle, err := devices.OpenDevRegKey(device, windows.DICS_FLAG_GLOBAL, 0, windows.DIREG_DEV, windows.KEY_QUERY_VALUE)
		if err != nil {
			continue
		}
		key := registry.Key(handle)
		name, _, err := key.GetStringValue("PortName")
		key.Close()
		if err != nil {
			continue
		}
		port, err := light.NormalizePort(name)
		if err != nil {
			continue
		}
		if _, duplicate := ports[port]; duplicate {
			return nil, fmt.Errorf("ambiguous USB serial port %s", port)
		}
		ports[port] = paths[0]
	}
}

// go.bug.st/serial prepends the Win32 device prefix to every name which
// doesn't already use it. Preserve the full USB interface link, changing
// only that prefix; a reassigned COM alias must never be opened on retry.
func serialInterfaceName(interfacePath string) (string, error) {
	path, ok := strings.CutPrefix(interfacePath, `\\?\`)
	if !ok || !strings.HasPrefix(strings.ToUpper(path), "USB#") {
		return "", fmt.Errorf("expected a USB device interface path, got %q", interfacePath)
	}
	return `\\.\` + path, nil
}

func openPL81Port(interfacePath string) (light.Port, error) {
	name, err := serialInterfaceName(interfacePath)
	if err != nil {
		return nil, err
	}
	mode := &serial.Mode{
		BaudRate: 115200,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
		// The proven 2026-08-08 probe ran on .NET SerialPort defaults: DTR
		// and RTS DEASSERTED. go.bug.st's default asserts both, and CH340
		// boards are often line-state sensitive - replicate the proven
		// configuration explicitly; trust neither stack's default.
		InitialStatusBits: &serial.ModemOutputBits{RTS: false, DTR: false},
	}
	port, err := serial.Open(name, mode)
	if err != nil {
		// Typically "access denied" if something else holds the port
		// exclusively (e.g. NEEWER Control Center).
		return nil, fmt.Errorf("open %s: %w", name, err)
	}
	if err := port.ResetInputBuffer(); err != nil {
		port.Close()
		return nil, err
	}
	if err := port.ResetOutputBuffer(); err != nil {
		port.Close()
		return nil, err
	}
	// Fix the poll timeout exactly once, here, before the port is shared:
	// v1.8.0 opens in NoTimeout mode (a Read would block forever), and
	// re-issuing SetCommTimeouts per read can race an in-flight Write.
	if err := port.SetReadTimeout(time.Second); err != nil {
		port.Close()
		return nil, err
	}
	return serialPort{port}, nil
}

// serialPort adapts go.bug.st/serial to light.Port. The 1 s read timeout
// was fixed once in openPL81Port, so a Read that returns (0, nil) on expiry
// matches the Port contract exactly - no per-read SetReadTimeout (which
// could race an in-flight Write via SetCommTimeouts).
type serialPort struct {
	p serial.Port
}

func (s serialPort) Write(b []byte) (int, error) { return s.p.Write(b) }

func (s serialPort) Read(b []byte) (int, error) { return s.p.Read(b) }

func (s serialPort) Close() error { return s.p.Close() }
