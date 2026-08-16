package light

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Defaults used when nothing has been persisted yet.
const (
	defaultBrightness = 100
	defaultKelvin     = 5000
)

// persisted is the on-disk shape of light-state.json.
type persisted struct {
	On         bool `json:"on"`         // last known power state
	Brightness int  `json:"brightness"` // last non-zero brightness (restore target)
	TempByte   int  `json:"temp_byte"`  // last temp byte
}

// State tracks the light's last-known condition. Like the mic's Tracker it
// is tri-state: known=false until the first echo/broadcast or optimistic
// Set. It additionally remembers the last non-zero brightness (the
// "previous look") and persists it, with the temp byte and power state, so
// on/toggle can restore the look across daemon restarts. There is no query
// command, so persisted values seed only the restore targets - never
// "known".
type State struct {
	mu         sync.Mutex
	path       string // persistence file; "" disables persistence
	known      bool
	brightness int       // 0-100; 0 means off
	temp       byte      // hardware temp step 0x00-0x12
	lastOn     int       // last non-zero brightness
	saved      persisted // last snapshot written to disk (skips no-op writes)
}

// NewState loads persisted restore targets from path if it exists. Missing
// or corrupt files silently fall back to defaults (100%, 5000K).
func NewState(path string) *State {
	s := &State{path: path, lastOn: defaultBrightness, temp: KelvinToByte(defaultKelvin)}
	if path == "" {
		return s
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var p persisted
	if json.Unmarshal(data, &p) != nil {
		return s
	}
	if p.Brightness > 0 && p.Brightness <= 100 {
		s.lastOn = p.Brightness
	}
	if p.TempByte >= 0 && p.TempByte <= maxTempByte {
		s.temp = byte(p.TempByte)
	}
	s.saved = p
	return s
}

// Set records a known state - from an echo, a knob broadcast, or
// optimistically after a successful write - and persists it if it changed.
// The returned error is a persistence failure only; the in-memory state is
// always updated.
func (s *State) Set(brightness int, temp byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.known = true
	s.brightness = brightness
	s.temp = temp
	if brightness > 0 {
		s.lastOn = brightness
	}
	return s.persistLocked()
}

func (s *State) persistLocked() error {
	if s.path == "" {
		return nil
	}
	p := persisted{On: s.brightness > 0, Brightness: s.lastOn, TempByte: int(s.temp)}
	if p == s.saved {
		return nil // no change since last write (knob turns won't spam the disk)
	}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(s.path, data, 0o644); err != nil {
		return err
	}
	s.saved = p
	return nil
}

// Status returns the tri-state condition. on is brightness > 0.
func (s *State) Status() (on bool, brightness int, temp byte, known bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.brightness > 0, s.brightness, s.temp, s.known
}

// TargetOn returns what "turn it on" should send: the last non-zero
// brightness and the current/persisted temp byte.
func (s *State) TargetOn() (brightness int, temp byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastOn, s.temp
}

// Snapshot returns the save-facing view of the whole state under ONE lock
// (R5-F4): power, the brightness a save records (the live brightness when
// on, the restore target when off - never 0), the temp byte, and
// known-ness. settingsSave reads each light through this single accessor
// instead of separate Status()/TargetOn() calls, so a mutation landing
// mid-snapshot changes the whole entry or none of it - never a hybrid of
// two looks (say, the off flag of the old state with the restore target of
// the new).
func (s *State) Snapshot() (on bool, brightness int, temp byte, known bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	on = s.brightness > 0
	brightness = s.brightness
	if !on {
		brightness = s.lastOn
	}
	return on, brightness, s.temp, s.known
}

// StatusString renders the UDP status reply: "unknown", "off", or
// "on <brightness>% <kelvin>K" (Kelvin is the quantized hardware step).
func (s *State) StatusString() string {
	on, b, temp, known := s.Status()
	if !known {
		return "unknown"
	}
	if !on {
		return "off"
	}
	return fmt.Sprintf("on %d%% %dK", b, ByteToKelvin(temp))
}
