package light

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
)

// Names, saved looks and cached states keep their original COM keys. None of
// them can attest to the identity of the device currently assigned that number.
const pairingFile = "light-pairs.json"

func loadPairings(dir string) (map[string]string, error) {
	pairs := make(map[string]string)
	if dir == "" {
		return pairs, errors.New("light pairing storage unavailable")
	}
	path := filepath.Join(dir, pairingFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return pairs, nil
	}
	if err == nil {
		err = json.Unmarshal(data, &pairs)
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if pairs == nil {
		return nil, fmt.Errorf("expected a pairing map in %s", path)
	}
	seen := make(map[string]bool, len(pairs))
	for port, device := range pairs {
		key := strings.ToUpper(device)
		if !portPattern.MatchString(port) || key == "" || seen[key] {
			return nil, fmt.Errorf("invalid pairing for %q in %s", port, path)
		}
		seen[key] = true
	}
	return pairs, nil
}

func (mm *MultiManager) changePairing(verb, target string) string {
	port, err := NormalizePort(target)
	if err != nil {
		return "error: " + err.Error()
	}
	var path string
	if verb == "pair" {
		devices, err := mm.enumerate()
		if err != nil {
			return "error: enumerate light candidates: " + err.Error()
		}
		path = devices[port]
		if path == "" {
			return "error: no compatible USB bridge present on " + port
		}
	}
	mm.mu.Lock()
	session, err := mm.setPairingLocked(port, path)
	mm.mu.Unlock()
	if err != nil {
		return "error: " + err.Error()
	}
	if session != nil && !mm.drain(session) {
		return "error: unpaired " + port + ", waiting for the driver to release the old connection"
	}
	mm.logger.Printf("light %s: %sed", port, verb)
	return verb + "ed " + port
}

// A running session always opens its captured interface path; even during
// debounce or a failed rescan it cannot reconnect via a recycled COM alias.
func (mm *MultiManager) setPairingLocked(port, path string) (*lightSession, error) {
	if mm.pairingErr != nil {
		return nil, mm.pairingErr
	}
	old := mm.pairings[port]
	if path != "" {
		if strings.EqualFold(old, path) {
			return nil, nil
		}
		if old != "" {
			return nil, fmt.Errorf("%s is paired to a different device; unpair it first", port)
		}
		for other, device := range mm.pairings {
			if strings.EqualFold(device, path) {
				return nil, fmt.Errorf("device is already paired as %s; unpair it first", other)
			}
		}
	} else if old == "" {
		return nil, fmt.Errorf("%s is not paired", port)
	}

	next := maps.Clone(mm.pairings)
	if path == "" {
		delete(next, port)
	} else {
		next[port] = path
	}
	if err := writeJSON(filepath.Join(mm.stateDir, pairingFile), next); err != nil {
		return nil, err
	}
	mm.pairings = next
	if path == "" {
		if s := mm.sessions[port]; s != nil {
			s.cancel()
			delete(mm.sessions, port)
			return s, nil
		}
	}
	return nil, nil
}
