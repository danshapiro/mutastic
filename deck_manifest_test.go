package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestDeckPluginManifest pins the OpenDeck manifest contract: required
// top-level fields (Name/Author/Version/Icon/Actions/OS — a missing one
// makes OpenDeck skip the plugin with only a warn log), the action
// identity that the profile edit and the runtime both depend on, and the
// EXTENSIONLESS image paths OpenDeck requires (its convert_icon appends
// .svg/@2x.png/.png itself; "icons/x.png" would resolve icons/x.png.png).
func TestDeckPluginManifest(t *testing.T) {
	raw, err := os.ReadFile("deck/com.danshapiro.mutastic.sdPlugin/manifest.json")
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m struct {
		Name, Author, Version, Icon, CodePathWin string
		OS                                       []struct{ Platform string }
		Actions                                  []struct {
			Name, UUID             string
			DisableAutomaticStates bool
			Controllers            []string
			States                 []struct{ Image string }
		}
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	for field, val := range map[string]string{"Name": m.Name, "Author": m.Author, "Version": m.Version, "Icon": m.Icon} {
		if val == "" {
			t.Errorf("manifest %s is empty (OpenDeck requires it)", field)
		}
	}
	if m.CodePathWin != "mutastic.exe" {
		t.Errorf("CodePathWin = %q, want mutastic.exe (flat layout, binary at plugin dir root)", m.CodePathWin)
	}
	if len(m.OS) != 1 || m.OS[0].Platform != "windows" {
		t.Errorf("OS = %+v, want exactly one windows entry", m.OS)
	}
	wantActions := map[string]struct {
		name   string
		images []string
	}{
		"com.danshapiro.mutastic.mute":  {"Mutastic Mute", []string{"icons/mutastic-mic", "icons/mutastic-mic-muted"}},
		"com.danshapiro.mutastic.light": {"Mutastic Lights", []string{"icons/mutastic-light-off", "icons/mutastic-light-on"}},
	}
	if len(m.Actions) != len(wantActions) {
		t.Fatalf("Actions has %d entries, want %d (mute + lights)", len(m.Actions), len(wantActions))
	}
	for _, a := range m.Actions {
		want, ok := wantActions[a.UUID]
		if !ok {
			t.Errorf("unexpected action UUID %q", a.UUID)
			continue
		}
		delete(wantActions, a.UUID)
		if a.Name != want.name {
			t.Errorf("action %s Name = %q, want %q", a.UUID, a.Name, want.name)
		}
		if !a.DisableAutomaticStates {
			t.Errorf("action %s: DisableAutomaticStates must be true: the plugin alone drives the icon", a.UUID)
		}
		if len(a.States) != len(want.images) {
			t.Fatalf("action %s: States has %d entries, want %d", a.UUID, len(a.States), len(want.images))
		}
		for i, st := range a.States {
			if st.Image != want.images[i] {
				t.Errorf("action %s States[%d].Image = %q, want %q", a.UUID, i, st.Image, want.images[i])
			}
			if strings.Contains(st.Image, ".png") {
				t.Errorf("action %s States[%d].Image = %q must be extensionless", a.UUID, i, st.Image)
			}
		}
	}
	for uuid := range wantActions {
		t.Errorf("manifest is missing action %s", uuid)
	}
	// The PNGs deploy.cmd installs next to this manifest must exist.
	for _, p := range []string{
		"deck/icons/mutastic-mic.png", "deck/icons/mutastic-mic-muted.png",
		"deck/icons/mutastic-light-on.png", "deck/icons/mutastic-light-off.png",
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("icon missing: %s: %v", p, err)
		}
	}
}
