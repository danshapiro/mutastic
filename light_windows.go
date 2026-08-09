//go:build windows

package main

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"

	"mutastic/internal/light"
)

// PL81 Pro = CH340 USB-serial bridge (docs/pl81-pro-serial-protocol.md).
// The enumerator reports VID/PID as hex strings.
const (
	pl81VID = "1A86"
	pl81PID = "7523"
)

// openPL81 finds the CH340 bridge by USB VID/PID - NEVER by COM number, the
// port name can change - and opens it at 115200 8N1 with both buffers
// flushed. The wake sequence is the session's job, not this function's.
// Every candidate port is logged so the log file doubles as a diagnostic
// record, mirroring openYetiX.
func openPL81(logger *log.Logger) (light.Port, error) {
	ports, err := enumerator.GetDetailedPortsList()
	if err != nil {
		return nil, err
	}
	var name string
	for _, p := range ports {
		logger.Printf("light: serial port: %s usb=%v vid=%s pid=%s", p.Name, p.IsUSB, p.VID, p.PID)
		if name == "" && p.IsUSB && strings.EqualFold(p.VID, pl81VID) && strings.EqualFold(p.PID, pl81PID) {
			name = p.Name
		}
	}
	if name == "" {
		return nil, errors.New("PL81 (CH340, VID 1A86 PID 7523) not found")
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
// was fixed once in openPL81, so a Read that returns (0, nil) on expiry
// matches the Port contract exactly - no per-read SetReadTimeout (which
// could race an in-flight Write via SetCommTimeouts).
type serialPort struct {
	p serial.Port
}

func (s serialPort) Write(b []byte) (int, error) { return s.p.Write(b) }

func (s serialPort) Read(b []byte) (int, error) { return s.p.Read(b) }

func (s serialPort) Close() error { return s.p.Close() }

// pl81Present reports whether a 1A86:7523 device is currently enumerated
// (SetupAPI, present devices only). The session loop uses it as its
// liveness fallback during long read silences, because the CH340 driver's
// surprise-removal error behavior is unverified. Enumeration failures
// count as present (fail open - never kill a session on an enumerator
// glitch).
func pl81Present() bool {
	ports, err := enumerator.GetDetailedPortsList()
	if err != nil {
		return true
	}
	for _, p := range ports {
		if p.IsUSB && strings.EqualFold(p.VID, pl81VID) && strings.EqualFold(p.PID, pl81PID) {
			return true
		}
	}
	return false
}
