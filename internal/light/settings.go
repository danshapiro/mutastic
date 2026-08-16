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
	"unicode/utf8"
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
	// (overwriting an existing name always fits), and LOAD enforces the
	// cap identically - an over-cap file is classified corrupt (R4-F4).
	// The cap exists together with the delete verb - without delete it
	// would fill permanently.
	// Headroom: 100 names x 43 bytes <= 4.3 KB < the 8192-byte client reply
	// read buffer, so a full store's list can never silently truncate.
	maxSettingsCount = 100
	// maxSettingsNameLen: with the 22-byte longest verb prefix
	// ("light settings delete ") the largest legal command is exactly 64
	// bytes; the daemon's UDP receive buffer is 128 (R7-F3: 2x headroom),
	// so an over-cap name arrives WHOLE and this cap rejects it with the
	// documented error on every platform - under the old 64-byte buffer a
	// 65-byte delete truncated to the 42-byte prefix name ON UNIX and
	// deleted it. Datagrams beyond 128 bytes still can't manufacture a
	// valid <=42-byte name (see serveUDP), so the cap stays the single
	// chokepoint that keeps every name reachable by every verb.
	maxSettingsNameLen = 42
)

// Store refusal/validation errors. Each error's text IS the wire reply
// tail (the handlers reply "error: " + it), so keep them contract-exact.
var (
	errSettingsDisabled = errors.New("settings persistence disabled")
	errSettingsCorrupt  = errors.New("settings store corrupt or unreadable")
	errSettingsCap      = errors.New("too many saved settings (max 100)")
	errUnknownSetting   = errors.New("unknown setting")
	// errSettingsInvalidEntry (R8-F5): Save refuses a name/snapshot that
	// violates the shared valid* predicates - e.g. a live state driven
	// out of range by a garbage inbound frame (the CCT parser feeds
	// pwr/brightness/temp bytes through unchecked). Persisting it would
	// write a file the next LOAD then classifies corrupt, taking the
	// whole store with it: the refusal keeps the file and the store
	// untouched instead.
	errSettingsInvalidEntry = errors.New("live state violates the saved-settings invariants (brightness 0-100, temp step, COM key, name grammar); refusing to save")
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
	// A parseable file must still be VALID (R2-F5 + R4-F4 + R5-F3): the
	// entry count stays at or under maxSettingsCount (an over-cap file
	// breaks the cap's headroom proof - 100 names x 43 bytes < the
	// 8192-byte reply read buffer - so its list could silently truncate),
	// every name satisfies the name grammar exactly as the verbs enforce
	// it (validStoreName: trimmed-form-equal - leading/trailing whitespace
	// is never meaningful - nonempty, no control bytes, at most
	// maxSettingsNameLen bytes, no case-insensitive "error:" prefix),
	// every entry holds a NON-EMPTY Lights map (an entryless snapshot
	// restores nothing, and the save path never produces one), and every
	// light entry itself is in range (R5-F3, validSavedLight). These run
	// through the shared valid* predicates (R8-F5) - THE SAME functions
	// Save runs - so load and save can never drift apart. ANY violation
	// classifies the whole file as corrupt: the store refuses everything
	// with the path-bearing wire string, and the file is preserved
	// untouched for the documented manual recovery.
	if len(m) > maxSettingsCount {
		s.corrupt = true
		return s
	}
	for name, entry := range m {
		if !validStoreName(name) || !validSavedSetting(entry) {
			s.corrupt = true
			return s
		}
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
// name. On any error - store refusal, the invariant refusal, the cap,
// persistence - the store is left UNCHANGED (NOTHING is persisted): the
// in-memory entry commits only after the write→close→replace sequence
// succeeds. Same-side invariant validation (R8-F5): the candidate
// name/entry must pass THE SAME predicates load enforces (validStoreName
// + validSavedSetting - factored so the two sides can never drift), or
// the save replies errSettingsInvalidEntry instead of writing a file the
// next load would classify corrupt.
func (s *SettingsStore) Save(name string, snap SavedSetting) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refusalLocked(); err != nil {
		return err
	}
	if !validStoreName(name) || !validSavedSetting(snap) {
		return errSettingsInvalidEntry
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
// sub-verb): internal whitespace (spaces - every OTHER control byte is
// rejected below) is preserved, leading/trailing whitespace is never
// meaningful, and "foo " IS "foo". Control bytes are rejected outright
// (R8-F3): newline would corrupt the newline-joined list reply, NUL would
// break the tray's Windows UTF16 menu conversion
// (syscall.UTF16PtrFromString returns EINVAL on an embedded NUL, so a
// NUL-bearing name could never be painted), and the rest of the C0/C1-adjacent
// set has no business crossing every surface the name traverses (wire,
// JSON, log, menu). Multi-byte UTF-8 printable runes remain allowed: a
// BYTE-level scan never touches them - continuation bytes are >= 0x80.
// The byte scan alone is not the whole grammar (R9-F2): the name must
// ALSO be well-formed UTF-8. A raw stray continuation byte (lone 0x80) or
// a truncated multi-byte sequence passes the scan, but every
// JSON/UTF16-carrying surface then renders a DIFFERENT string - Go's
// encoding/json coerces invalid UTF-8 to U+FFFD on marshal AND unmarshal
// (the persisted file's decoded name would differ from the in-memory key
// and from what list replies), and the tray's Windows UTF-16 conversion
// coerces a third rendering, so one stored name would mean one thing to
// the click and another to the file. Rejecting at the grammar keeps a
// name the same string across wire, file, and menu. Because JSON decode
// itself coerces, this clause can only ever fire pre-persistence (the
// wire/save path); load classification via the shared predicate sees only
// already-coerced names and is unchanged.
// Names case-insensitively starting with "error:" are likewise rejected
// (clients parse error:-prefixed replies as failures). The two guarantees
// the grammar buys: every accepted name is exactly ONE wire-list line
// (it cannot forge list lines), and every accepted name is
// Windows-UTF16-representable (the EINVAL-on-NUL class cannot happen).
func validateSettingsName(name string) string {
	if name == "" || strings.HasPrefix(strings.ToLower(name), "error:") {
		return "error: invalid settings name"
	}
	// Any byte < 0x20 (NUL, newline, tab, CR, ESC, ...) or 0x7F (DEL).
	for i := 0; i < len(name); i++ {
		if name[i] < 0x20 || name[i] == 0x7F {
			return "error: invalid settings name"
		}
	}
	if !utf8.ValidString(name) {
		return "error: invalid settings name"
	}
	if len(name) > maxSettingsNameLen {
		return fmt.Sprintf("error: settings name too long (max %d bytes)", maxSettingsNameLen)
	}
	return ""
}

// validStoreName, validSavedSetting, and validSavedLight are the SINGLE
// source of the store's validity invariants (R8-F5): LOAD
// (NewSettingsStore) and SAVE (SettingsStore.Save) both run THESE
// predicates, so the two sides can never drift apart and a save can never
// persist content that load would refuse as corrupt (a self-corrupting
// file: it would persist fine, then classify the whole store corrupt at
// the next load).
func validStoreName(name string) bool {
	return name == strings.TrimSpace(name) && validateSettingsName(name) == ""
}

// validSavedSetting enforces the per-entry invariants: a NON-EMPTY Lights
// map (an entryless snapshot restores nothing, and the live save path
// refuses to produce one) whose every light itself is in range.
func validSavedSetting(entry SavedSetting) bool {
	if len(entry.Lights) == 0 {
		return false
	}
	for key, ls := range entry.Lights {
		if !validSavedLight(key, ls) {
			return false
		}
	}
	return true
}

// validSavedLight enforces the per-light invariants (R5-F3): 0 <=
// Brightness <= 100, a TempByte inside the firmware's 0x00..maxTempByte
// Kelvin step table (frame.go; see CCT/KelvinToByte), keyed by a plausible
// COM-port path in the codebase's own form (portPattern, the COM<n> shape
// NormalizePort/enumeration produce - an invented key could never address
// hardware on apply).
func validSavedLight(key string, ls SavedLightState) bool {
	return portPattern.MatchString(key) && ls.Brightness >= 0 && ls.Brightness <= 100 && ls.TempByte <= maxTempByte
}
