package light

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// SavedLightState is one light's look inside a SavedSetting: power state,
// brightness (0-100), and the hardware temp byte (render via ByteToKelvin).
type SavedLightState struct {
	On         bool `json:"on"`
	Brightness int  `json:"brightness"`
	TempByte   byte `json:"temp_byte"`
}

// SavedSetting is a named snapshot of the fleet's known-state lights,
// keyed by COM port path ONLY: registry names are mutable ("light name")
// and must never decide which hardware an entry restores.
type SavedSetting struct {
	Lights map[string]SavedLightState `json:"lights"`
}

const (
	// maxSettingsCount caps the store: a NEW name past the cap is refused
	// (overwriting an existing name always fits). The cap exists together
	// with the delete verb - without delete it would fill permanently.
	// Headroom: 100 names x 43 bytes <= 4.3 KB < the 8192-byte client reply
	// read buffer, so a full store's list can never silently truncate.
	maxSettingsCount = 100
	// maxSettingsNameLen: the daemon's 64-byte UDP receive buffer minus the
	// 22-byte longest verb prefix ("light settings delete ") - a longer
	// name would save fine, then silently truncate on delete/apply and
	// could never be removed or applied.
	maxSettingsNameLen = 42
)

// Store refusal/validation errors. Each error's text IS the wire reply
// tail (the handlers reply "error: " + it), so keep them contract-exact.
var (
	errSettingsDisabled = errors.New("settings persistence disabled")
	errSettingsCorrupt  = errors.New("settings store corrupt or unreadable")
	errSettingsCap      = errors.New("too many saved settings (max 100)")
	errUnknownSetting   = errors.New("unknown setting")
)

// SettingsStore persists named light snapshots beside the registry at
// <stateDir>/light-settings.json, mirroring its conventions (json.Marshal +
// MkdirAll + 0644, one in-memory mutex) with two deliberate differences:
// the write is a write→close→replace sequence (never the Registry's plain
// os.WriteFile - a torn whole-file rewrite must never silently lose every
// saved name), and a corrupt or unreadable file DISABLES the store instead
// of silently starting empty.
type SettingsStore struct {
	mu      sync.Mutex
	path    string // "" disables the store entirely
	byName  map[string]SavedSetting
	corrupt bool // load saw an unreadable/unparseable file: refuse everything
}

// NewSettingsStore loads the store at path: "" path -> disabled store;
// missing file -> empty; CORRUPT or UNREADABLE file -> disabled-corrupt.
// A disabled-corrupt store refuses every mutation and NO save ever
// replaces the broken file; the recovery is stop the daemon, rename/delete
// the file, then start the daemon.
func NewSettingsStore(path string) *SettingsStore {
	s := &SettingsStore{path: path, byName: map[string]SavedSetting{}}
	if path == "" {
		return s
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			s.corrupt = true // present but unreadable
		}
		return s
	}
	var m map[string]SavedSetting
	if json.Unmarshal(data, &m) != nil {
		s.corrupt = true
		return s
	}
	if m != nil {
		s.byName = m
	}
	return s
}

// Enabled reports whether the store accepts work: false when disabled
// ("" path) OR disabled-corrupt.
func (s *SettingsStore) Enabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.path != "" && !s.corrupt
}

// refusal returns the store's refusal error, or nil when healthy. All
// verbs reply the same single-line "error: " + refusal, so every client
// parses it with the existing error:-prefixed => not-ok rule.
func (s *SettingsStore) refusal() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refusalLocked()
}

func (s *SettingsStore) refusalLocked() error {
	if s.path == "" {
		return errSettingsDisabled
	}
	if s.corrupt {
		return fmt.Errorf("%w: %s", errSettingsCorrupt, s.path)
	}
	return nil
}

// Save stores snap under name, overwriting an existing entry by exact
// name. On any error - store refusal, the cap, persistence - the store is
// left UNCHANGED: the in-memory entry commits only after the
// write→close→replace sequence succeeds.
func (s *SettingsStore) Save(name string, snap SavedSetting) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refusalLocked(); err != nil {
		return err
	}
	if _, exists := s.byName[name]; !exists && len(s.byName) >= maxSettingsCount {
		return errSettingsCap
	}
	next := make(map[string]SavedSetting, len(s.byName)+1)
	for n, v := range s.byName {
		next[n] = v
	}
	next[name] = snap
	if err := s.writeLocked(next); err != nil {
		return err
	}
	s.byName = next
	return nil
}

// Delete removes name and persists the store with the same
// write→close→replace discipline as Save. An unknown name or a refused
// store errors and leaves the store unchanged.
func (s *SettingsStore) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refusalLocked(); err != nil {
		return err
	}
	if _, ok := s.byName[name]; !ok {
		return fmt.Errorf("%w %q", errUnknownSetting, name)
	}
	next := make(map[string]SavedSetting, len(s.byName)-1)
	for n, v := range s.byName {
		if n != name {
			next[n] = v
		}
	}
	if err := s.writeLocked(next); err != nil {
		return err
	}
	s.byName = next
	return nil
}

// List returns the sorted names; empty when disabled or disabled-corrupt
// (the wire verb replies the refusal string on those stores instead).
func (s *SettingsStore) List() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" || s.corrupt {
		return nil
	}
	names := make([]string, 0, len(s.byName))
	for n := range s.byName {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Get returns the snapshot saved under name.
func (s *SettingsStore) Get(name string) (SavedSetting, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, ok := s.byName[name]
	return snap, ok
}

// writeLocked persists the candidate store: marshal, write a temp file in
// the SAME DIRECTORY to completion and CLOSE it, then a same-volume
// replace over the target (os.Rename; on Windows os.Rename is MoveFileEx
// with REPLACE_EXISTING - this does NOT claim a Windows rename is atomic).
// A crash at any point leaves the intact OLD store or the intact NEW one,
// possibly plus an orphan .tmp - never a truncated store. The safety
// argument is the write→close→replace ORDER, portable across platforms.
func (s *SettingsStore) writeLocked(byName map[string]SavedSetting) error {
	data, err := json.Marshal(byName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, s.path)
}

// validateSettingsName enforces the name grammar shared identically by
// save, apply, and delete, returning ""/no error text when valid. Names
// arrive already OUTER-trimmed (serveUDP and HandleCommand TrimSpace the
// whole command, and the settings handler trims the raw suffix after the
// sub-verb): internal whitespace is preserved, leading/trailing whitespace
// is never meaningful, and "foo " IS "foo". Newlines are rejected outright
// (they would corrupt the newline-joined list reply), as are names
// case-insensitively starting with "error:" (clients parse error:-prefixed
// replies as failures).
func validateSettingsName(name string) string {
	if name == "" || strings.ContainsAny(name, "\r\n") || strings.HasPrefix(strings.ToLower(name), "error:") {
		return "error: invalid settings name"
	}
	if len(name) > maxSettingsNameLen {
		return fmt.Sprintf("error: settings name too long (max %d bytes)", maxSettingsNameLen)
	}
	return ""
}
