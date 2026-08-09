package light

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRegistryAssignResolveNameFor(t *testing.T) {
	r := NewRegistry(filepath.Join(t.TempDir(), "light-names.json"))
	if err := r.Assign("com4", "Desk"); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"desk", "DESK", "com4", "COM4"} {
		p, ok := r.Resolve(target)
		if !ok || p != "COM4" {
			t.Fatalf("Resolve(%q) = %q, %v; want COM4, true", target, p, ok)
		}
	}
	if got := r.NameFor("COM4"); got != "desk" {
		t.Fatalf("NameFor(COM4) = %q, want desk", got)
	}
	if got := r.NameFor("COM7"); got != "" {
		t.Fatalf("NameFor(COM7) = %q, want empty", got)
	}
}

func TestRegistryResolvesPortLiterals(t *testing.T) {
	r := NewRegistry("")
	if p, ok := r.Resolve("com9"); !ok || p != "COM9" {
		t.Fatalf("Resolve(com9) = %q, %v; want COM9, true", p, ok)
	}
	if _, ok := r.Resolve("nope"); ok {
		t.Fatal("Resolve(nope) should fail")
	}
}

func TestRegistryReassignMovesName(t *testing.T) {
	r := NewRegistry("")
	if err := r.Assign("COM4", "desk"); err != nil {
		t.Fatal(err)
	}
	if err := r.Assign("COM7", "desk"); err != nil {
		t.Fatal(err)
	}
	if p, _ := r.Resolve("desk"); p != "COM7" {
		t.Fatalf("desk -> %q, want COM7", p)
	}
	if got := r.NameFor("COM4"); got != "" {
		t.Fatalf("COM4 still named %q, want unnamed", got)
	}
}

func TestRegistryRenamingPortReplacesOldName(t *testing.T) {
	r := NewRegistry("")
	r.Assign("COM4", "desk")
	r.Assign("COM4", "key")
	if got := r.NameFor("COM4"); got != "key" {
		t.Fatalf("NameFor(COM4) = %q, want key", got)
	}
	if _, ok := r.Resolve("desk"); ok {
		t.Fatal("old name desk should be gone")
	}
}

func TestRegistryUnname(t *testing.T) {
	r := NewRegistry("")
	r.Assign("COM4", "desk")
	name, err := r.Unname("desk")
	if err != nil || name != "desk" {
		t.Fatalf("Unname(desk) = %q, %v; want desk, nil", name, err)
	}
	r.Assign("COM4", "desk")
	name, err = r.Unname("com4") // clearing by port works too
	if err != nil || name != "desk" {
		t.Fatalf("Unname(com4) = %q, %v; want desk, nil", name, err)
	}
	if _, err := r.Unname("desk"); err == nil {
		t.Fatal("Unname of unknown target should error")
	}
}

func TestRegistryValidation(t *testing.T) {
	r := NewRegistry("")
	for _, name := range []string{"", "9lives", "has space", "com7", "COM7", "this-name-is-way-too-long", "upper!"} {
		if err := r.Assign("COM4", name); err == nil {
			t.Fatalf("Assign accepted bad name %q", name)
		}
	}
	if err := r.Assign("USB0", "desk"); err == nil {
		t.Fatal("Assign accepted bad port USB0")
	}
}

func TestRegistryPersistsAndToleratesCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "light-names.json")
	r := NewRegistry(path)
	if err := r.Assign("COM4", "desk"); err != nil {
		t.Fatal(err)
	}
	r2 := NewRegistry(path)
	if p, ok := r2.Resolve("desk"); !ok || p != "COM4" {
		t.Fatalf("reloaded Resolve(desk) = %q, %v; want COM4, true", p, ok)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	r3 := NewRegistry(path)
	if _, ok := r3.Resolve("desk"); ok {
		t.Fatal("corrupt file should start empty")
	}

	// Test Unname persistence: unname a binding and verify it's gone on reload
	path2 := filepath.Join(t.TempDir(), "light-names-unname.json")
	r4 := NewRegistry(path2)
	if err := r4.Assign("COM4", "desk"); err != nil {
		t.Fatal(err)
	}
	if err := r4.Assign("COM7", "key"); err != nil {
		t.Fatal(err)
	}
	// Unname "desk" and verify it's gone
	if _, err := r4.Unname("desk"); err != nil {
		t.Fatal(err)
	}
	// Reload and verify "desk" is gone but "key" remains
	r5 := NewRegistry(path2)
	if _, ok := r5.Resolve("desk"); ok {
		t.Fatal("Unnnamed binding 'desk' should not persist")
	}
	if p, ok := r5.Resolve("key"); !ok || p != "COM7" {
		t.Fatalf("other binding 'key' should still exist; got %q, %v", p, ok)
	}
}

func TestRegistryLoaderEnforcesBijection(t *testing.T) {
	// Test that NewRegistry enforces bijection: rejects duplicate ports and port-shaped names.
	// Load JSON with duplicate ports and a port-shaped name; verify only one valid binding survives.
	path := filepath.Join(t.TempDir(), "light-names-bijection.json")
	
	// Write JSON with: desk->COM4, key->COM4 (duplicate port), com7->COM5 (port-shaped name)
	jsonData := `{"desk":"COM4","key":"COM4","com7":"COM5"}`
	if err := os.WriteFile(path, []byte(jsonData), 0o644); err != nil {
		t.Fatal(err)
	}
	
	r := NewRegistry(path)
	all := r.All()
	
	// Should have exactly 1 binding (one of desk/key survived, plus port-shaped names rejected)
	if len(all) != 1 {
		t.Fatalf("loader should keep exactly 1 valid binding; got %d: %v", len(all), all)
	}
	
	// The port-shaped name "com7" should not be in the registry
	if _, ok := all["com7"]; ok {
		t.Fatal("port-shaped name 'com7' should have been rejected")
	}
	
	// One of desk or key should exist (map iteration makes WHICH arbitrary)
	if p, ok := all["desk"]; ok {
		if p != "COM4" {
			t.Fatalf("if 'desk' survived, it should be COM4, got %q", p)
		}
	} else if p, ok := all["key"]; ok {
		if p != "COM4" {
			t.Fatalf("if 'key' survived, it should be COM4, got %q", p)
		}
	} else {
		t.Fatal("either 'desk' or 'key' should have survived (got neither)")
	}
}
