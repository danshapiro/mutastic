package deckplugin

import (
	"strings"
	"testing"
)

func TestParseArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    Config
		wantErr bool
	}{
		{
			// Exact argv OpenDeck passes (plugins/mod.rs): single-dash flags,
			// values as separate elements, -info last with raw JSON.
			name: "real OpenDeck argv",
			args: []string{"-port", "57116", "-pluginUUID", "com.danshapiro.mutastic.sdPlugin", "-registerEvent", "registerPlugin", "-info", `{"application":{"version":"2.13.1"}}`},
			want: Config{Port: 57116, PluginUUID: "com.danshapiro.mutastic.sdPlugin", RegisterEvent: "registerPlugin", Info: `{"application":{"version":"2.13.1"}}`},
		},
		{
			name: "info is optional",
			args: []string{"-port", "57117", "-pluginUUID", "x.sdPlugin", "-registerEvent", "registerPlugin"},
			want: Config{Port: 57117, PluginUUID: "x.sdPlugin", RegisterEvent: "registerPlugin"},
		},
		{name: "flag without value", args: []string{"-port"}, wantErr: true},
		{name: "bad port", args: []string{"-port", "nope", "-pluginUUID", "x", "-registerEvent", "e"}, wantErr: true},
		{name: "unknown flag", args: []string{"-port", "1", "-pluginUUID", "x", "-registerEvent", "e", "-bogus", "v"}, wantErr: true},
		{name: "missing required flags", args: []string{"-port", "57116"}, wantErr: true},
		{name: "empty argv", args: nil, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseArgs(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseArgs(%v) = %+v, want error", tt.args, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseArgs(%v): unexpected error: %v", tt.args, err)
			}
			if got != tt.want {
				t.Fatalf("ParseArgs(%v) = %+v, want %+v", tt.args, got, tt.want)
			}
		})
	}
}

func TestDecodeEventWillAppear(t *testing.T) {
	// Verbatim wire shape from OpenDeck's outbound/will_appear.rs serializer.
	frame := `{"event":"willAppear","action":"com.danshapiro.mutastic.mute","context":"sd-A00DA6141I07PW.Default.Keypad.5.0","device":"sd-A00DA6141I07PW","payload":{"settings":{},"coordinates":{"row":1,"column":2},"controller":"Keypad","state":0,"isInMultiAction":false}}`
	ev, err := DecodeEvent([]byte(frame))
	if err != nil {
		t.Fatalf("DecodeEvent: %v", err)
	}
	if ev.Event != "willAppear" {
		t.Errorf("Event = %q, want willAppear", ev.Event)
	}
	if ev.Action != "com.danshapiro.mutastic.mute" {
		t.Errorf("Action = %q, want com.danshapiro.mutastic.mute", ev.Action)
	}
	if ev.Context != "sd-A00DA6141I07PW.Default.Keypad.5.0" {
		t.Errorf("Context = %q, want the verbatim dotted string", ev.Context)
	}
	if ev.Device != "sd-A00DA6141I07PW" {
		t.Errorf("Device = %q, want sd-A00DA6141I07PW", ev.Device)
	}
}

func TestDecodeEventKeyDown(t *testing.T) {
	frame := `{"event":"keyDown","action":"com.danshapiro.mutastic.mute","context":"sd-A00DA6141I07PW.Default.Keypad.5.0","device":"sd-A00DA6141I07PW","payload":{"settings":{},"coordinates":{"row":1,"column":2},"controller":"Keypad","state":0,"isInMultiAction":false}}`
	ev, err := DecodeEvent([]byte(frame))
	if err != nil {
		t.Fatalf("DecodeEvent: %v", err)
	}
	if ev.Event != "keyDown" {
		t.Errorf("Event = %q, want keyDown", ev.Event)
	}
}

func TestDecodeEventRejectsGarbage(t *testing.T) {
	if _, err := DecodeEvent([]byte(`not json`)); err == nil {
		t.Error("DecodeEvent(not json) succeeded, want error")
	}
	if _, err := DecodeEvent([]byte(`{"payload":{}}`)); err == nil {
		t.Error(`DecodeEvent without "event" field succeeded, want error`)
	}
}

func TestEncodeRegister(t *testing.T) {
	got := string(EncodeRegister("registerPlugin", "com.danshapiro.mutastic.sdPlugin"))
	want := `{"event":"registerPlugin","uuid":"com.danshapiro.mutastic.sdPlugin"}`
	if got != want {
		t.Fatalf("EncodeRegister = %s, want %s", got, want)
	}
}

func TestEncodeSetState(t *testing.T) {
	got := string(EncodeSetState("sd-A00DA6141I07PW.Default.Keypad.5.0", 1))
	want := `{"event":"setState","context":"sd-A00DA6141I07PW.Default.Keypad.5.0","payload":{"state":1}}`
	if got != want {
		t.Fatalf("EncodeSetState = %s, want %s", got, want)
	}
	if !strings.Contains(string(EncodeSetState("a.b.c.5.0", 0)), `"state":0`) {
		t.Fatal("EncodeSetState must carry state 0 explicitly (omitempty would silently drop it)")
	}
}
