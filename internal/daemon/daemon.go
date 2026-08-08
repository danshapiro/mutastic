package daemon

import (
	"errors"
	"log"
	"sync"
	"time"

	"mutastic/internal/proto"
)

// Device is the minimal HID handle the daemon needs. Implementations must
// return (0, nil) from ReadWithTimeout when the timeout elapses with no
// data; any non-nil error is treated as "device gone" and triggers a
// reconnect.
type Device interface {
	Write(p []byte) (int, error)
	ReadWithTimeout(p []byte, timeout time.Duration) (int, error)
	Close() error
}

var errNoDevice = errors.New("no device")

// Daemon holds shared daemon state: the tracked mute state, the current
// device handle, and the logger.
type Daemon struct {
	Track  Tracker
	Logger *log.Logger

	mu  sync.Mutex
	dev Device
}

// New returns a Daemon that logs to logger.
func New(logger *log.Logger) *Daemon {
	return &Daemon{Logger: logger}
}

// SetDevice installs the current device handle (nil while disconnected).
func (d *Daemon) SetDevice(dev Device) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dev = dev
}

// WriteReport sends one output report. Writes are serialized by the mutex;
// per the protocol doc, the returned byte count is NOT asserted on (Windows
// hidapi reports 64 for a 65-byte buffer).
func (d *Daemon) WriteReport(report []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dev == nil {
		return errNoDevice
	}
	_, err := d.dev.Write(report)
	return err
}

// HandleCommand executes one UDP text command and returns the reply.
// Replies are exactly: "muted", "unmuted", "unknown", or "error: <reason>".
func (d *Daemon) HandleCommand(cmd string) string {
	switch cmd {
	case "status":
		muted, known := d.Track.Status()
		if !known {
			return "unknown"
		}
		if muted {
			return "muted"
		}
		return "unmuted"
	case "mute":
		return d.setMute(true)
	case "unmute":
		return d.setMute(false)
	case "toggle":
		muted, known := d.Track.Status()
		target := true // unknown state: default to mute (safe for a pedal press)
		if known {
			target = !muted
		}
		return d.setMute(target)
	default:
		return "error: unknown command"
	}
}

func (d *Daemon) setMute(muted bool) string {
	payload := []byte("0")
	if muted {
		payload = []byte("1")
	}
	if err := d.WriteReport(proto.EncodeCommand(proto.OpMute, payload)); err != nil {
		return "error: " + err.Error()
	}
	d.Track.Set(muted) // optimistic; the 0x20 echo confirms it
	if muted {
		return "muted"
	}
	return "unmuted"
}
