package light

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Registry is a persistent, bijective name<->port map for addressing
// lights. Names are case-insensitive identifiers ("desk"); ports are
// Windows COM names ("COM4"). Reassigning a name moves it; naming a port
// replaces its old name. Backed by light-names.json ("" disables
// persistence; missing or corrupt files silently start empty, mirroring
// NewState).
type Registry struct {
	mu    sync.Mutex
	path  string
	names map[string]string // lowercase name -> canonical port ("COM4")
}

var (
	// namePattern: 1-16 chars of a-z 0-9 '-', starting with a letter.
	namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,15}$`)
	// portPattern matches canonical (uppercased) Windows COM names.
	portPattern = regexp.MustCompile(`^COM[0-9]+$`)
)

// NewRegistry loads light-names.json from path if it exists.
func NewRegistry(path string) *Registry {
	r := &Registry{path: path, names: map[string]string{}}
	if path == "" {
		return r
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return r
	}
	var m map[string]string
	if json.Unmarshal(data, &m) != nil {
		return r
	}
	for name, port := range m {
		if namePattern.MatchString(name) && portPattern.MatchString(port) {
			r.names[name] = port
		}
	}
	return r
}

// NormalizePort validates and canonicalizes a COM port name
// ("com4" -> "COM4").
func NormalizePort(s string) (string, error) {
	p := strings.ToUpper(s)
	if !portPattern.MatchString(p) {
		return "", fmt.Errorf("invalid port %q (want COM<n>)", s)
	}
	return p, nil
}

// Assign binds name to port (case-insensitive), replacing any existing
// name for that port and moving the name if it was bound elsewhere.
func (r *Registry) Assign(port, name string) error {
	p, err := NormalizePort(port)
	if err != nil {
		return err
	}
	n := strings.ToLower(name)
	if portPattern.MatchString(strings.ToUpper(n)) {
		return errors.New("invalid name: looks like a COM port")
	}
	if !namePattern.MatchString(n) {
		return errors.New("invalid name: want 1-16 chars of a-z 0-9 '-', starting with a letter")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for existing, bound := range r.names {
		if bound == p {
			delete(r.names, existing)
		}
	}
	r.names[n] = p
	return r.saveLocked()
}

// Unname removes a binding by name or port, returning the removed name.
func (r *Registry) Unname(target string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := strings.ToLower(target)
	if _, ok := r.names[n]; ok {
		delete(r.names, n)
		return n, r.saveLocked()
	}
	if p, err := NormalizePort(target); err == nil {
		for name, bound := range r.names {
			if bound == p {
				delete(r.names, name)
				return name, r.saveLocked()
			}
		}
	}
	return "", fmt.Errorf("no name for %q", target)
}

// Resolve maps a target - a name or a COM port, case-insensitive - to the
// canonical port name. Port literals always resolve (the caller decides
// whether that port is actually attached).
func (r *Registry) Resolve(target string) (string, bool) {
	if p, err := NormalizePort(target); err == nil {
		return p, true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.names[strings.ToLower(target)]
	return p, ok
}

// NameFor returns the name bound to port ("" when unnamed).
func (r *Registry) NameFor(port string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for n, p := range r.names {
		if p == port {
			return n
		}
	}
	return ""
}

// All returns a copy of every name->port binding.
func (r *Registry) All() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.names))
	for n, p := range r.names {
		out[n] = p
	}
	return out
}

func (r *Registry) saveLocked() error {
	if r.path == "" {
		return nil
	}
	data, err := json.Marshal(r.names)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(r.path, data, 0o644)
}
