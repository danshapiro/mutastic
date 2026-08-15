package light

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// cctStep decodes one tag-0x02 frame's (brightness, temp) payload.
type cctStep struct{ brightness, temp int }

// cctWrites decodes the CCT frames a fakePort recorded from index `from`,
// skipping the raw wake bytes.
func cctWrites(p *fakePort, from int) []cctStep {
	var steps []cctStep
	for i := from; i < p.writeCount(); i++ {
		w := p.write(i)
		if len(w) == 8 && w[0] == 0x3A && w[1] == TagCCT && w[2] == 0x03 {
			steps = append(steps, cctStep{int(w[4]), int(w[5])})
		}
	}
	return steps
}

func TestSavedSettingsStoreSaveListGetRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "light-settings.json")
	s := NewSettingsStore(path)
	if !s.Enabled() {
		t.Fatal("a fresh store at a real path must be enabled")
	}
	if got := s.List(); len(got) != 0 {
		t.Fatalf("empty store list = %v, want none", got)
	}
	if _, ok := s.Get("nope"); ok {
		t.Fatal("Get on an unknown name must miss")
	}
	look := SavedSetting{Lights: map[string]SavedLightState{
		"COM4": {On: true, Brightness: 50, TempByte: 9},
		"COM7": {On: false, Brightness: 30, TempByte: 0},
	}}
	if err := s.Save("beta", look); err != nil {
		t.Fatalf("Save beta: %v", err)
	}
	if err := s.Save("alpha", look); err != nil {
		t.Fatalf("Save alpha: %v", err)
	}
	if got := s.List(); !reflect.DeepEqual(got, []string{"alpha", "beta"}) {
		t.Fatalf("List = %v, want sorted [alpha beta]", got)
	}
	if got, ok := s.Get("alpha"); !ok || !reflect.DeepEqual(got, look) {
		t.Fatalf("Get(alpha) = %+v, %v; want the saved snapshot", got, ok)
	}
	// Overwrite by exact name: still a single entry, holding the new snapshot.
	other := SavedSetting{Lights: map[string]SavedLightState{
		"COM4": {On: true, Brightness: 100, TempByte: 18},
	}}
	if err := s.Save("alpha", other); err != nil {
		t.Fatalf("overwrite alpha: %v", err)
	}
	if got := s.List(); !reflect.DeepEqual(got, []string{"alpha", "beta"}) {
		t.Fatalf("List after overwrite = %v, want still [alpha beta]", got)
	}
	if got, _ := s.Get("alpha"); !reflect.DeepEqual(got, other) {
		t.Fatalf("Get(alpha) after overwrite = %+v, want the new snapshot", got)
	}
}

func TestSavedSettingsStorePersistedAcrossReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "light-settings.json")
	s := NewSettingsStore(path)
	look := SavedSetting{Lights: map[string]SavedLightState{
		"COM4": {On: true, Brightness: 50, TempByte: 9},
	}}
	if err := s.Save("look", look); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// A second store over the same path sees the snapshot.
	reloaded := NewSettingsStore(path)
	if got, ok := reloaded.Get("look"); !ok || !reflect.DeepEqual(got, look) {
		t.Fatalf("reloaded Get(look) = %+v, %v; want the persisted snapshot", got, ok)
	}
	if got := reloaded.List(); !reflect.DeepEqual(got, []string{"look"}) {
		t.Fatalf("reloaded List = %v, want [look]", got)
	}

	// "" path: the store is disabled and every op is a refused no-op.
	disabled := NewSettingsStore("")
	if disabled.Enabled() {
		t.Fatal("NewSettingsStore(\"\") must be disabled")
	}
	if err := disabled.Save("x", look); err == nil {
		t.Fatal("disabled Save must error")
	}
	if err := disabled.Delete("x"); err == nil {
		t.Fatal("disabled Delete must error")
	}
	if got := disabled.List(); len(got) != 0 {
		t.Fatalf("disabled List = %v, want empty", got)
	}

	// A corrupt file makes the store disabled-corrupt: every op refuses,
	// the in-memory List reads empty, and no save ever replaces the file.
	cdir := t.TempDir()
	cpath := filepath.Join(cdir, "light-settings.json")
	garbage := []byte("{definitely not json")
	if err := os.WriteFile(cpath, garbage, 0o644); err != nil {
		t.Fatal(err)
	}
	corrupt := NewSettingsStore(cpath)
	if corrupt.Enabled() {
		t.Fatal("a corrupt store must not report enabled")
	}
	if got := corrupt.List(); len(got) != 0 {
		t.Fatalf("corrupt in-memory List = %v, want empty", got)
	}
	if err := corrupt.Save("x", look); err == nil {
		t.Fatal("corrupt Save must error")
	}
	if _, ok := corrupt.Get("x"); ok {
		t.Fatal("a refused corrupt Save must not land in memory")
	}
	if err := corrupt.Delete("x"); err == nil {
		t.Fatal("corrupt Delete must error")
	}

	// The same refusal reaches every wire verb (list included) through a
	// MultiManager built over that directory.
	mm, ctx := newTestMulti(t, newFakeFleet(), cdir)
	mm.rescan(ctx)
	wantCorrupt := "error: settings store corrupt or unreadable: " + cpath
	for _, cmd := range []string{"settings save x", "settings apply x", "settings delete x", "settings list"} {
		if got := mm.HandleCommand(cmd); got != wantCorrupt {
			t.Errorf("%q = %q, want %q", cmd, got, wantCorrupt)
		}
	}
	if data, err := os.ReadFile(cpath); err != nil || !bytes.Equal(data, garbage) {
		t.Fatalf("corrupt file = %q (err %v); it must survive untouched", data, err)
	}
}

func TestSavedSettingsSaveWriteFailure(t *testing.T) {
	fastTimings(t)
	dir := t.TempDir()
	fleet := newFakeFleet("COM4")
	mm, ctx := newTestMulti(t, fleet, dir)
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4")
	sessionManager(t, mm, "COM4").state.Set(50, 0)
	if got := mm.HandleCommand("settings save base"); got != `saved "base" (1 lights)` {
		t.Fatalf("setup save = %q", got)
	}
	// Point the store at an unwritable location: replace the whole stateDir
	// with a plain FILE so the persist's MkdirAll/temp-write fails.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := mm.HandleCommand("settings save blocked")
	if !strings.HasPrefix(got, "error: settings save failed:") {
		t.Fatalf("failed save reply = %q, want prefix %q", got, "error: settings save failed:")
	}
	// The existing store is unchanged: the pre-existing name still
	// lists/gets, and the new name never landed in memory or on disk.
	if got := mm.HandleCommand("settings list"); got != "base" {
		t.Fatalf("list after failed save = %q, want %q", got, "base")
	}
	if _, ok := mm.settings.Get("base"); !ok {
		t.Fatal("pre-existing name must still Get after a failed save")
	}
	if _, ok := mm.settings.Get("blocked"); ok {
		t.Fatal("a failed save must not land in memory")
	}
}

func TestSavedSettingsSaveAndListWireVerbs(t *testing.T) {
	fastTimings(t)
	dir := t.TempDir()
	fleet := newFakeFleet("COM4")
	mm, ctx := newTestMulti(t, fleet, dir)
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4")
	sessionManager(t, mm, "COM4").state.Set(60, 9)

	if got := mm.HandleCommand("settings list"); got != "" {
		t.Fatalf("empty list = %q, want the empty string", got)
	}
	if got := mm.HandleCommand("settings save beta"); got != `saved "beta" (1 lights)` {
		t.Fatalf("save beta = %q", got)
	}
	if got := mm.HandleCommand("settings save alpha"); got != `saved "alpha" (1 lights)` {
		t.Fatalf("save alpha = %q", got)
	}
	if got := mm.HandleCommand("settings list"); got != "alpha\nbeta" {
		t.Fatalf("list = %q, want sorted newline-joined %q", got, "alpha\nbeta")
	}
	// Overwrite keeps one entry.
	if got := mm.HandleCommand("settings save beta"); got != `saved "beta" (1 lights)` {
		t.Fatalf("overwrite beta = %q", got)
	}
	if got := mm.HandleCommand("settings list"); got != "alpha\nbeta" {
		t.Fatalf("list after overwrite = %q, want %q", got, "alpha\nbeta")
	}
	// The on-disk JSON holds both names.
	var onDisk map[string]SavedSetting
	readJSON(t, filepath.Join(dir, "light-settings.json"), &onDisk)
	if len(onDisk) != 2 {
		t.Fatalf("on-disk store holds %d names, want 2: %v", len(onDisk), onDisk)
	}
	for _, name := range []string{"alpha", "beta"} {
		e := onDisk[name].Lights["COM4"]
		if e != (SavedLightState{On: true, Brightness: 60, TempByte: 9}) {
			t.Fatalf("on-disk %s = %+v, want the COM4 snapshot", name, e)
		}
	}
	// Malformed sub-verbs get a usage error.
	const usage = "error: usage: light settings <save|list|apply|delete> [name]"
	for _, cmd := range []string{"settings", "settings frobnicate", "settings list extra"} {
		if got := mm.HandleCommand(cmd); got != usage {
			t.Errorf("%q = %q, want %q", cmd, got, usage)
		}
	}
}

func TestSavedSettingsSnapshotKeysByPortOnly(t *testing.T) {
	fastTimings(t)
	dir := t.TempDir()
	fleet := newFakeFleet("COM4", "COM7")
	mm, ctx := newTestMulti(t, fleet, dir)
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4", "COM7")
	if got := mm.HandleCommand("name COM4 desk"); got != "named COM4 desk" {
		t.Fatalf("name = %q", got)
	}
	sessionManager(t, mm, "COM4").state.Set(47, 0)
	sessionManager(t, mm, "COM7").state.Set(30, 9)
	sessionManager(t, mm, "COM7").state.Set(0, 9) // off; restore target (30, 9)

	if got := mm.HandleCommand("settings save look"); got != `saved "look" (2 lights)` {
		t.Fatalf("save = %q", got)
	}
	snap, ok := mm.settings.Get("look")
	if !ok {
		t.Fatal("look missing from the store")
	}
	if len(snap.Lights) != 2 {
		t.Fatalf("snapshot has %d lights, want 2: %+v", len(snap.Lights), snap.Lights)
	}
	// Named or not, every entry is keyed by the COM PORT PATH - never by
	// the (mutable) registry name.
	if e, ok := snap.Lights["COM4"]; !ok || e != (SavedLightState{On: true, Brightness: 47, TempByte: 0}) {
		t.Fatalf("COM4 entry = %+v, %v; want on 47@0", e, ok)
	}
	if _, bad := snap.Lights["desk"]; bad {
		t.Fatal("a registry name must never key an entry")
	}
	// An off light saves its RESTORE-TARGET brightness, not 0.
	if e, ok := snap.Lights["COM7"]; !ok || e != (SavedLightState{On: false, Brightness: 30, TempByte: 9}) {
		t.Fatalf("COM7 entry = %+v, %v; want off with restore target (30, 9)", e, ok)
	}
	// The reloaded file matches.
	reloaded := NewSettingsStore(filepath.Join(dir, "light-settings.json"))
	if got, ok := reloaded.Get("look"); !ok || !reflect.DeepEqual(got, snap) {
		t.Fatalf("reloaded snapshot = %+v, %v; want %+v", got, ok, snap)
	}

	// A registry-name change BETWEEN save and apply must not redirect which
	// hardware an entry restores: the entry follows the PORT, the reply
	// label picks up the current name.
	if got := mm.HandleCommand("unname desk"); got != "unnamed desk" {
		t.Fatalf("unname = %q", got)
	}
	if got := mm.HandleCommand("name COM7 desk"); got != "named COM7 desk" {
		t.Fatalf("rename = %q", got)
	}
	sessionManager(t, mm, "COM4").state.Set(1, 18) // disturb both lights
	sessionManager(t, mm, "COM7").state.Set(99, 0)
	from4, from7 := fleet.port("COM4").writeCount(), fleet.port("COM7").writeCount()
	if got, want := mm.HandleCommand("settings apply look"), "COM4: on 47% 2900K\nCOM7 desk: off"; got != want {
		t.Fatalf("apply after rename = %q, want %q", got, want)
	}
	if got := fleet.port("COM4").writeCount() - from4; got != 3 {
		t.Fatalf("COM4 frames = %d, want 3 (the entry restored ITS port)", got)
	}
	if got := fleet.port("COM7").writeCount() - from7; got != 3 {
		t.Fatalf("COM7 frames = %d, want 3 (the renamed light keeps its own entry)", got)
	}
}

func TestSavedSettingsSaveOmitsUnknownStateLights(t *testing.T) {
	fastTimings(t)
	dir := t.TempDir()
	fleet := newFakeFleet("COM4", "COM7")
	mm, ctx := newTestMulti(t, fleet, dir)
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4", "COM7")
	// COM4 known; COM7 stays connected-but-unknown (no echo, no command).
	sessionManager(t, mm, "COM4").state.Set(50, 0)

	if got := mm.HandleCommand("settings save partial"); got != `saved "partial" (1 lights)` {
		t.Fatalf("save = %q", got)
	}
	snap, _ := mm.settings.Get("partial")
	if len(snap.Lights) != 1 {
		t.Fatalf("snapshot = %+v, want only COM4", snap.Lights)
	}
	if _, bad := snap.Lights["COM7"]; bad {
		t.Fatal("an unknown-state light must be omitted from the snapshot")
	}

	// An all-unknown fleet refuses, and the store stays untouched.
	dir2 := t.TempDir()
	fleet2 := newFakeFleet("COM4", "COM7")
	mm2, ctx2 := newTestMulti(t, fleet2, dir2)
	mm2.rescan(ctx2)
	waitConnected(t, mm2, "COM4", "COM7")
	if got := mm2.HandleCommand("settings save nothing"); got != "error: no known light state to save" {
		t.Fatalf("all-unknown save = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir2, "light-settings.json")); !os.IsNotExist(err) {
		t.Fatalf("store file exists after the refused save (err %v); nothing may persist", err)
	}
	if got := mm2.HandleCommand("settings list"); got != "" {
		t.Fatalf("list after refused save = %q, want empty", got)
	}
}

func TestSavedSettingsSaveExcludesDisconnectedCachedState(t *testing.T) {
	fastTimings(t)
	dir := t.TempDir()
	fleet := newFakeFleet("COM4", "COM7")
	mm, ctx := newTestMulti(t, fleet, dir)
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4", "COM7")
	sessionManager(t, mm, "COM4").state.Set(50, 0)
	sessionManager(t, mm, "COM7").state.Set(70, 3) // known, then unplugged below

	fleet.set("COM4") // unplug COM7; its state stays CACHED on the dead session
	mm.rescan(ctx)    // miss 1: debounced
	mm.rescan(ctx)    // miss 2: torn down - gone from the fan-out's session set

	got := mm.HandleCommand("settings save survivor")
	if got != `saved "survivor" (1 lights)` {
		t.Fatalf("save = %q; the cached-but-gone light must not count in N", got)
	}
	snap, _ := mm.settings.Get("survivor")
	if len(snap.Lights) != 1 {
		t.Fatalf("snapshot = %+v, want only the live light", snap.Lights)
	}
	if _, bad := snap.Lights["COM7"]; bad {
		t.Fatal("a disconnected light's merely-cached state must be excluded")
	}
	if _, ok := snap.Lights["COM4"]; !ok {
		t.Fatal("the live connected known light must be saved")
	}
}

func TestSavedSettingsApplyRestoresLiveStateInOrder(t *testing.T) {
	fastTimings(t)
	dir := t.TempDir()
	fleet := newFakeFleet("COM4", "COM7")
	mm, ctx := newTestMulti(t, fleet, dir)
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4", "COM7")
	m4 := sessionManager(t, mm, "COM4")
	m7 := sessionManager(t, mm, "COM7")
	m4.state.Set(47, 0) // on 47% 2900K - the ON entry
	m7.state.Set(30, 0) // on 30%...
	m7.state.Set(0, 0)  // ...then off with restore target (30, 0) - the OFF entry
	if got := mm.HandleCommand("settings save look"); got != `saved "look" (2 lights)` {
		t.Fatalf("save = %q", got)
	}
	// Disturb the fleet: different on/brightness/temp/restore targets.
	m4.state.Set(60, 18) // on 60% 7000K
	m7.state.Set(80, 9)
	m7.state.Set(0, 9) // off with restore target (80, 9)

	fp4, fp7 := fleet.port("COM4"), fleet.port("COM7")
	from4, from7 := fp4.writeCount(), fp7.writeCount()
	got := mm.HandleCommand("settings apply look")
	// Deterministic reply: lines preallocated in keys-sorted order.
	if want := "COM4: on 47% 2900K\nCOM7: off"; got != want {
		t.Fatalf("apply reply = %q, want %q", got, want)
	}
	// Per-light frame sequences. The power-state frame is ALWAYS LAST: the
	// ON entry plays on -> brightness -> temp; the OFF entry brightness ->
	// temp -> off, its brightness/temp writes briefly ENERGIZING the light
	// (firmware behavior) before the off frame parks it. Cross-light
	// interleaving is deliberately NOT asserted - the fan-out is parallel.
	if got, want := cctWrites(fp4, from4), []cctStep{{60, 18}, {47, 18}, {47, 0}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("COM4 (ON entry) frames = %v, want on->brightness->temp %v", got, want)
	}
	if got, want := cctWrites(fp7, from7), []cctStep{{30, 9}, {30, 0}, {0, 0}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("COM7 (OFF entry) frames = %v, want brightness->temp->off %v", got, want)
	}
	// The disturbed fleet is restored, including COM7's restore targets.
	if got := m4.HandleCommand("status"); got != "on 47% 2900K" {
		t.Fatalf("COM4 status = %q, want the saved look", got)
	}
	if got := m7.HandleCommand("status"); got != "off" {
		t.Fatalf("COM7 status = %q, want off", got)
	}
	if b, temp := m7.state.TargetOn(); b != 30 || temp != 0 {
		t.Fatalf("COM7 restore target = (%d, %d), want the saved (30, 0)", b, temp)
	}
}

func TestSavedSettingsApplySkipsUnreachableKeys(t *testing.T) {
	fastTimings(t)
	dir := t.TempDir()
	fleet := newFakeFleet("COM4", "COM7")
	mm, ctx := newTestMulti(t, fleet, dir)
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4", "COM7")
	sessionManager(t, mm, "COM4").state.Set(47, 0)
	sessionManager(t, mm, "COM7").state.Set(80, 9)
	if got := mm.HandleCommand("settings save mix"); got != `saved "mix" (2 lights)` {
		t.Fatalf("save = %q", got)
	}
	// COM7 unplugged (or moved USB ports so it answers on a different COM):
	// its saved port key has no live session.
	fleet.set("COM4")
	mm.rescan(ctx) // miss 1: debounced
	mm.rescan(ctx) // miss 2: torn down

	from4 := fleet.port("COM4").writeCount()
	got := mm.HandleCommand("settings apply mix")
	want := "COM4: on 47% 2900K\n" + `error: light "COM7": unreachable, skipped`
	if got != want {
		t.Fatalf("apply = %q, want %q", got, want)
	}
	if got := fleet.port("COM4").writeCount() - from4; got != 3 {
		t.Fatalf("COM4 frames = %d, want 3 (live-port entries still apply)", got)
	}
}

func TestSavedSettingsApplyErrorShapes(t *testing.T) {
	fastTimings(t)
	dir := t.TempDir()
	fleet := newFakeFleet("COM4")
	mm, ctx := newTestMulti(t, fleet, dir)
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4")
	sessionManager(t, mm, "COM4").state.Set(50, 0)
	if got := mm.HandleCommand("settings save look"); got != `saved "look" (1 lights)` {
		t.Fatalf("save = %q", got)
	}
	if got := mm.HandleCommand("settings apply nope"); got != `error: unknown setting "nope"` {
		t.Fatalf("unknown-name apply = %q", got)
	}
	// Zero lights connected: a known name still cannot apply.
	fleet.set()
	mm.rescan(ctx) // miss 1: debounced
	mm.rescan(ctx) // miss 2: torn down
	if got := mm.HandleCommand("settings apply look"); got != "error: no lights connected" {
		t.Fatalf("zero-light apply = %q", got)
	}
}

func TestSavedSettingsNameValidation(t *testing.T) {
	fastTimings(t)
	dir := t.TempDir()
	fleet := newFakeFleet("COM4")
	mm, ctx := newTestMulti(t, fleet, dir)
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4")
	sessionManager(t, mm, "COM4").state.Set(50, 0)

	// Empty, newline-containing, and error:-prefixed (both cases) names are
	// rejected identically for save, apply, AND delete.
	const invalid = "error: invalid settings name"
	for _, verb := range []string{"save", "apply", "delete"} {
		for _, name := range []string{"", "foo\nbar", "error:foo", "ERROR:foo"} {
			cmd := "settings " + verb + " " + name
			if got := mm.HandleCommand(cmd); got != invalid {
				t.Errorf("%q = %q, want %q", cmd, got, invalid)
			}
		}
	}
	// 43 bytes is over; 42 (the inclusive cap) is accepted by all verbs.
	tooLong := strings.Repeat("a", 43)
	const wantTooLong = "error: settings name too long (max 42 bytes)"
	for _, verb := range []string{"save", "apply", "delete"} {
		if got := mm.HandleCommand("settings " + verb + " " + tooLong); got != wantTooLong {
			t.Errorf("settings %s <43-byte name> = %q, want %q", verb, got, wantTooLong)
		}
	}
	longEnough := strings.Repeat("a", 42)
	if got, want := mm.HandleCommand("settings save "+longEnough), `saved "`+longEnough+`" (1 lights)`; got != want {
		t.Fatalf("save 42-byte name = %q, want %q", got, want)
	}
	if got, want := mm.HandleCommand("settings apply "+longEnough), "COM4: on 50% 2900K"; got != want {
		t.Fatalf("apply 42-byte name = %q, want %q", got, want)
	}
	if got, want := mm.HandleCommand("settings delete "+longEnough), `deleted "`+longEnough+`"`; got != want {
		t.Fatalf("delete 42-byte name = %q, want %q", got, want)
	}
	// Rejected saves never land in the list.
	if got := mm.HandleCommand("settings list"); got != "" {
		t.Fatalf("list after rejected saves = %q, want empty", got)
	}
	// Names are trimmed at the pipeline boundary: "foo " and "foo" are the
	// SAME setting (a later save overwrites; no whitespace variants exist).
	if got := mm.HandleCommand("settings save foo "); got != `saved "foo" (1 lights)` {
		t.Fatalf("save padded name = %q", got)
	}
	if got := mm.HandleCommand("settings save foo"); got != `saved "foo" (1 lights)` {
		t.Fatalf("save bare name = %q", got)
	}
	if got := mm.HandleCommand("settings list"); got != "foo" {
		t.Fatalf("list = %q, want exactly one %q entry", got, "foo")
	}
	// Internal whitespace is preserved.
	if got := mm.HandleCommand("settings save movie mode"); got != `saved "movie mode" (1 lights)` {
		t.Fatalf("save multi-word name = %q", got)
	}
	if got := mm.HandleCommand("settings list"); got != "foo\nmovie mode" {
		t.Fatalf("list = %q, want %q", got, "foo\nmovie mode")
	}
}

func TestSavedSettingsCapAndDeleteWireVerbs(t *testing.T) {
	fastTimings(t)
	dir := t.TempDir()
	fleet := newFakeFleet("COM4")
	mm, ctx := newTestMulti(t, fleet, dir)
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4")
	sessionManager(t, mm, "COM4").state.Set(50, 0)

	// 100 distinct saves succeed.
	for i := 0; i < 100; i++ {
		name := fmt.Sprintf("n%03d", i)
		if got, want := mm.HandleCommand("settings save "+name), fmt.Sprintf("saved %q (1 lights)", name); got != want {
			t.Fatalf("save %s = %q, want %q", name, got, want)
		}
	}
	// The 101st NEW name is refused...
	if got, want := mm.HandleCommand("settings save one001"), "error: too many saved settings (max 100)"; got != want {
		t.Fatalf("101st save = %q, want %q", got, want)
	}
	// ...while overwriting an existing name always fits, even at the cap.
	if got := mm.HandleCommand("settings save n005"); got != `saved "n005" (1 lights)` {
		t.Fatalf("overwrite at cap = %q", got)
	}
	if got := len(mm.settings.List()); got != 100 {
		t.Fatalf("store holds %d names, want 100", got)
	}

	// Delete validates the name identically to save/apply.
	if got := mm.HandleCommand("settings delete error:x"); got != "error: invalid settings name" {
		t.Fatalf("delete invalid name = %q", got)
	}
	if got := mm.HandleCommand("settings delete " + strings.Repeat("a", 43)); got != "error: settings name too long (max 42 bytes)" {
		t.Fatalf("delete too-long name = %q", got)
	}
	if got := mm.HandleCommand("settings delete nope"); got != `error: unknown setting "nope"` {
		t.Fatalf("delete unknown = %q", got)
	}
	if got := mm.HandleCommand("settings delete n005"); got != `deleted "n005"` {
		t.Fatalf("delete = %q", got)
	}
	if got := len(mm.settings.List()); got != 99 {
		t.Fatalf("store holds %d names after delete, want 99", got)
	}
	// A freed slot admits a NEW name again.
	if got := mm.HandleCommand("settings save freed"); got != `saved "freed" (1 lights)` {
		t.Fatalf("save after delete = %q", got)
	}
	// The deletion is persisted across a reload.
	reloaded := NewSettingsStore(filepath.Join(dir, "light-settings.json"))
	if _, ok := reloaded.Get("n005"); ok {
		t.Fatal("a deleted name must stay deleted after reload")
	}
	if _, ok := reloaded.Get("freed"); !ok {
		t.Fatal("the reloaded store must hold the newly saved name")
	}
	if got := len(reloaded.List()); got != 100 {
		t.Fatalf("reloaded store holds %d names, want 100", got)
	}

	// Delete on a disabled or disabled-corrupt store replies that store's
	// refusal, not a lookup result.
	mmOff, ctxOff := newTestMulti(t, newFakeFleet(), "")
	mmOff.rescan(ctxOff)
	if got := mmOff.HandleCommand("settings delete n000"); got != "error: settings persistence disabled" {
		t.Fatalf("disabled delete = %q", got)
	}
	cdir := t.TempDir()
	cpath := filepath.Join(cdir, "light-settings.json")
	if err := os.WriteFile(cpath, []byte("{garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	mmCorrupt, ctxCorrupt := newTestMulti(t, newFakeFleet(), cdir)
	mmCorrupt.rescan(ctxCorrupt)
	if got, want := mmCorrupt.HandleCommand("settings delete n000"), "error: settings store corrupt or unreadable: "+cpath; got != want {
		t.Fatalf("corrupt delete = %q, want %q", got, want)
	}
}

func TestSavedSettingsDisabledStore(t *testing.T) {
	fastTimings(t)
	fleet := newFakeFleet("COM4")
	mm, ctx := newTestMulti(t, fleet, "") // "" stateDir: the store is disabled
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4")
	sessionManager(t, mm, "COM4").state.Set(50, 0)
	// Every verb - list included - replies the single-line refusal, never
	// an empty success.
	for _, cmd := range []string{"settings save x", "settings apply x", "settings delete x", "settings list"} {
		if got, want := mm.HandleCommand(cmd), "error: settings persistence disabled"; got != want {
			t.Errorf("%q = %q, want %q", cmd, got, want)
		}
	}
}
