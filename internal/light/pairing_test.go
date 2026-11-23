package light

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Exercise discovery, the wire commands, persistence and the final argument to
// the serial opener. A rejected device must never even receive a wake write.
func TestDiscoveryDoesNotAdoptUnpairedBridges(t *testing.T) {
	fastTimings(t)
	dir := t.TempDir()
	reg := NewRegistry("")
	if err := reg.Assign("COM3", "old-name"); err != nil {
		t.Fatal(err)
	}
	devices := map[string]string{"COM3": "laser", "COM4": "desk-panel", "COM19": "other-bridge"}
	scan := func() (map[string]string, error) { return maps.Clone(devices), nil }
	var mu sync.Mutex
	var opened []string
	open := func(path string) (Port, error) {
		mu.Lock()
		defer mu.Unlock()
		opened = append(opened, path)
		return newFakePort(), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mm := NewMultiManager(testLogger(), dir, reg, scan, open)
	defer mm.stopAll()
	mm.rescan(ctx)
	mm.rescan(ctx)
	if got := mm.HandleCommand("list"); got != "COM3 old-name disconnected" {
		t.Fatalf("unpaired bridges after rescans: %s", got)
	}
	if got := mm.HandleCommand("pair COM8"); !strings.HasPrefix(got, "error:") {
		t.Fatal(got)
	}
	if got := mm.HandleCommand("pair com4"); got != "paired COM4" {
		t.Fatal(got)
	}
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4")
	if got := mm.HandleCommand("@COM4 brightness 36"); !strings.HasPrefix(got, "on 36%") {
		t.Fatalf("paired panel: %s", got)
	}
	mm.stopAll()

	restarted := NewMultiManager(testLogger(), dir, reg, scan, open)
	defer restarted.stopAll()
	devices["COM4"] = "replacement-bridge"
	restarted.rescan(ctx)
	if got := restarted.HandleCommand("list"); got != "COM3 old-name disconnected\nCOM4 - disconnected" {
		t.Fatalf("reassigned COM4 after restart: %s", got)
	}
	if got := restarted.HandleCommand("pair COM4"); !strings.HasPrefix(got, "error:") {
		t.Fatal("silently replaced existing pairing: " + got)
	}
	devices["COM4"] = "desk-panel"
	restarted.rescan(ctx)
	waitConnected(t, restarted, "COM4")
	if got := restarted.HandleCommand("unpair COM4"); got != "unpaired COM4" {
		t.Fatal(got)
	}
	restarted.rescan(ctx)
	if got := restarted.HandleCommand("@COM4 on"); !strings.Contains(got, "not connected") {
		t.Fatal(got)
	}
	again := NewMultiManager(testLogger(), dir, reg, scan, open)
	defer again.stopAll()
	again.rescan(ctx)
	if got := again.HandleCommand("list"); got != "COM3 old-name disconnected" {
		t.Fatalf("unpair did not persist: %s", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(opened) != 2 || opened[0] != "desk-panel" || opened[1] != "desk-panel" {
		t.Fatalf("serial opens = %v", opened)
	}
}

func TestOpenRetriesUseOriginalDevicePath(t *testing.T) {
	fastTimings(t)
	devices := map[string]string{"COM4": "panel"}
	scan := func() (map[string]string, error) { return maps.Clone(devices), nil }
	attempts := make(chan string, 8)
	open := func(path string) (Port, error) {
		attempts <- path
		return nil, errors.New("device gone")
	}
	mm := NewMultiManager(testLogger(), t.TempDir(), NewRegistry(""), scan, open)
	defer mm.stopAll()
	if got := mm.HandleCommand("pair COM4"); got != "paired COM4" {
		t.Fatal(got)
	}
	nextAttempt := func() string {
		t.Helper()
		select {
		case path := <-attempts:
			return path
		case <-time.After(time.Second):
			t.Fatal("no open attempt")
			return ""
		}
	}
	mm.rescan(context.Background())
	if path := nextAttempt(); path != "panel" {
		t.Fatal(path)
	}
	devices["COM4"] = "different-device"
	mm.rescan(context.Background()) // debounced before the session is cancelled
	if path := nextAttempt(); path != "panel" {
		t.Fatalf("retry switched targets: %s", path)
	}
}

func TestBrokenPairingStoreRefusesConnectionsAndWrites(t *testing.T) {
	for _, data := range []string{`{`, `null`, `{"COM4":""}`, `{"COM4":"path","COM9":"PATH"}`} {
		dir := t.TempDir()
		path := filepath.Join(dir, pairingFile)
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		scan := func() (map[string]string, error) { return map[string]string{"COM4": "path"}, nil }
		mm := NewMultiManager(testLogger(), dir, NewRegistry(""), scan, nil)
		mm.rescan(context.Background())
		if got := mm.HandleCommand("list"); got != "no lights known" {
			t.Fatal(got)
		}
		if got := mm.HandleCommand("pair COM4"); !strings.HasPrefix(got, "error:") {
			t.Fatal(got)
		}
		if kept, err := os.ReadFile(path); err != nil || string(kept) != data {
			t.Fatalf("corrupt store overwritten: %q, %v", kept, err)
		}
	}
}
