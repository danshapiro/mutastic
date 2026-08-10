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
	if len(m.Actions) != 1 {
		t.Fatalf("Actions has %d entries, want 1", len(m.Actions))
	}
	a := m.Actions[0]
	if a.UUID != "com.danshapiro.mutastic.mute" {
		t.Errorf("action UUID = %q, want com.danshapiro.mutastic.mute", a.UUID)
	}
	if a.Name != "Mutastic Mute" {
		t.Errorf("action Name = %q, want Mutastic Mute", a.Name)
	}
	if !a.DisableAutomaticStates {
		t.Error("DisableAutomaticStates must be true: the plugin alone drives the icon")
	}
	if len(a.States) != 2 {
		t.Fatalf("States has %d entries, want 2 (0 = live, 1 = muted)", len(a.States))
	}
	wantImages := []string{"icons/mutastic-mic", "icons/mutastic-mic-muted"}
	for i, st := range a.States {
		if st.Image != wantImages[i] {
			t.Errorf("States[%d].Image = %q, want %q", i, st.Image, wantImages[i])
		}
		if strings.Contains(st.Image, ".png") {
			t.Errorf("States[%d].Image = %q must be extensionless", i, st.Image)
		}
	}
	// The PNGs deploy.cmd installs next to this manifest must exist.
	for _, p := range []string{"deck/icons/mutastic-mic.png", "deck/icons/mutastic-mic-muted.png"} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("icon missing: %s: %v", p, err)
		}
	}
}
