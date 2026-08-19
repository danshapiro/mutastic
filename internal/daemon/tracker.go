// Package daemon implements the mutastic resident daemon: mute-state
// tracking, HID session management, and the UDP command server.
package daemon

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"

	"mutastic/internal/proto"
)

// Tracker holds the last known hardware mute state. The zero value is
// usable and reports known=false until the first Apply or Set.
type Tracker struct {
	mu     sync.Mutex
	known  bool
	muted  bool
	logger *log.Logger
	// persistPath, when non-empty, writes the state across daemon restarts.
	persistPath string
}

// SetPersistPath enables cross-restart persistence. If path is non-empty
// and the file exists, the tracker is seeded from it immediately. Writes
// happen inline on transitions; I/O errors are logged to logger and
// swallowed (the tracker still works — it just forgets again on the next
// restart).
func (t *Tracker) SetPersistPath(path string, logger *log.Logger) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.persistPath = path
	t.logger = logger
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var state struct {
		Muted bool `json:"muted"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		t.warnf("mic state persist: undecodable %s: %v", path, err)
		return
	}
	t.known = true
	t.muted = state.Muted
}

// Apply updates the state from a physical mute-button event
// (0x21 DeviceMute only). It returns true iff the event mutated tracked
// state.
//
// 0x20 SoftwareMute events are deliberately NOT applied here, even though
// their value byte can look decodable. On this firmware, 0x20 is a
// zero-payload command echo: the byte at offset 9 is a constant tag, not
// state (docs/yeti-x-hid-protocol.md open question 2). That tag has been
// observed as both 0x0b (undecodable, harmlessly ignored) and 0x00 (which
// decodes as "unmuted") across firmware/log samples. Trusting it resets
// the optimistically-tracked state to unmuted right after every outbound
// mute command, so consecutive pedal toggles all send "mute" and the mic
// can never be unmuted from the pedal -- a production regression this
// fixes. Do not "fix" this by decoding 0x20 more cleverly: the byte
// carries no state at all, on any firmware revision observed so far.
func (t *Tracker) Apply(e proto.Event) bool {
	if e.Op != proto.EvtDeviceMute {
		return false
	}
	muted, ok := proto.MutedFromValue(e.Value)
	if !ok {
		return false
	}
	t.Set(muted)
	return true
}

// Set records a known mute state (used optimistically after a successful
// outbound mute command; the device echo then confirms it).
func (t *Tracker) Set(muted bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.known = true
	t.muted = muted
	t.persistLocked()
}

// persistLocked writes the tracked state to disk; persistence failures are
// ignored (the state is best-effort — the hardware cannot be queried).
func (t *Tracker) persistLocked() {
	if t.persistPath == "" {
		return
	}
	raw, _ := json.Marshal(struct {
		Muted bool `json:"muted"`
	}{Muted: t.muted})
	_ = os.MkdirAll(filepath.Dir(t.persistPath), 0o755)
	tmp := t.persistPath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, t.persistPath); err != nil {
		t.warnf("mic state persist: rename failed: %v", err)
	}
}

// warnf logs a best-effort persistence warning; a nil logger drops it.
func (t *Tracker) warnf(format string, args ...any) {
	if t.logger != nil {
		t.logger.Printf(format, args...)
	}
}

// Reset drops the tracked state back to UNKNOWN (R8-F2): the belief the
// tracker holds stops being premise-worthy when the mic told us something
// we could not read (a 0x21 event with an undecodable value byte) or when
// the device session that belief came from is gone (a fresh session binds
// a device whose true state cannot be read - there is no state query).
// Conditional verbs then refuse with "flipped unknown" instead of acting
// on stale truth, until a real event or a successful verb re-establishes
// it.
func (t *Tracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.known = false
	t.muted = false
}

// Status returns the current state; known is false if no mute event or Set
// has been seen yet.
func (t *Tracker) Status() (muted bool, known bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.muted, t.known
}
