// Package deckplugin implements the OpenDeck (Elgato Stream Deck SDK)
// plugin protocol for the mutastic mute button: registration over a
// WebSocket, inbound event decoding, and outbound setState encoding,
// plus the state machine that keeps the key icon in sync with the
// daemon's true mute state. The package is platform-free; the real
// WebSocket, UDP client, F24 injector, and log file are injected from
// package main (deckplugin.go) — the same pattern as internal/daemon's
// Device/CommandHandler/KeyInjector.
package deckplugin

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// Config is the launch configuration OpenDeck passes on the command line:
//
//	mutastic.exe -port 57116 -pluginUUID com.danshapiro.mutastic.sdPlugin -registerEvent registerPlugin -info {...}
//
// PluginUUID is the plugin DIRECTORY name (OpenDeck's plugin identity);
// RegisterEvent is the event name to send in the register frame (always
// "registerPlugin" today — use the given value, never hardcode). Info is
// captured for completeness and unused.
type Config struct {
	Port          int
	PluginUUID    string
	RegisterEvent string
	Info          string
}

// ParseArgs parses Elgato-style plugin argv: single-dash flags with the
// value as the NEXT argv element (never -port=N). args excludes the
// program name (and the optional "deckplugin" subcommand word). Unknown
// flags are errors so a mangled launch fails loudly in the log instead
// of half-working.
func ParseArgs(args []string) (Config, error) {
	var cfg Config
	for i := 0; i < len(args); i += 2 {
		if i+1 >= len(args) {
			return Config{}, fmt.Errorf("flag %s has no value", args[i])
		}
		val := args[i+1]
		switch args[i] {
		case "-port":
			p, err := strconv.Atoi(val)
			if err != nil {
				return Config{}, fmt.Errorf("bad -port %q: %v", val, err)
			}
			cfg.Port = p
		case "-pluginUUID":
			cfg.PluginUUID = val
		case "-registerEvent":
			cfg.RegisterEvent = val
		case "-info":
			cfg.Info = val
		default:
			return Config{}, fmt.Errorf("unknown flag %q", args[i])
		}
	}
	if cfg.Port == 0 || cfg.PluginUUID == "" || cfg.RegisterEvent == "" {
		return Config{}, fmt.Errorf("missing required flags: need -port, -pluginUUID, -registerEvent (got port=%d uuid=%q event=%q)", cfg.Port, cfg.PluginUUID, cfg.RegisterEvent)
	}
	return cfg, nil
}

// Event is the envelope of one inbound frame from OpenDeck. The payload
// is deliberately not modeled — this plugin only needs the envelope.
// Context is an opaque token ("<device>.<profile>.<controller>.<pos>.<index>")
// that MUST be echoed back verbatim in setState: OpenDeck re-parses it
// and silently drops messages whose context doesn't round-trip.
type Event struct {
	Event   string `json:"event"`
	Action  string `json:"action"`
	Context string `json:"context"`
	Device  string `json:"device"`
}

// DecodeEvent decodes one inbound frame. Events the caller doesn't
// handle still decode fine; a missing "event" field is an error.
func DecodeEvent(data []byte) (Event, error) {
	var ev Event
	if err := json.Unmarshal(data, &ev); err != nil {
		return Event{}, fmt.Errorf("decode event: %w", err)
	}
	if ev.Event == "" {
		return Event{}, fmt.Errorf("decode event: missing \"event\" field in %.120s", data)
	}
	return ev, nil
}

type registerMsg struct {
	Event string `json:"event"`
	UUID  string `json:"uuid"`
}

// EncodeRegister builds the FIRST frame the plugin must send after the
// WebSocket connects: {"event":"registerPlugin","uuid":"<dir name>"}.
// A malformed or wrong-uuid register is OpenDeck's #1 silent failure
// mode: the socket is never added to its registry and the plugin
// receives nothing, with no error anywhere.
func EncodeRegister(event, uuid string) []byte {
	data, _ := json.Marshal(registerMsg{Event: event, UUID: uuid}) // marshal of plain strings cannot fail
	return data
}

type setStatePayload struct {
	State int `json:"state"`
}

type setStateMsg struct {
	Event   string          `json:"event"`
	Context string          `json:"context"`
	Payload setStatePayload `json:"payload"`
}

// EncodeSetState builds the setState frame that drives the key icon:
// {"event":"setState","context":C,"payload":{"state":N}}. OpenDeck
// bounds-checks state against the instance's states (a 2-state action
// accepts only 0 and 1) and authorizes context against the registered
// uuid; failures are silent no-ops on its side.
func EncodeSetState(context string, state int) []byte {
	data, _ := json.Marshal(setStateMsg{Event: "setState", Context: context, Payload: setStatePayload{State: state}}) // cannot fail
	return data
}
