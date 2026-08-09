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
}
