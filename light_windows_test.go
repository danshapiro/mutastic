package main

import "testing"

func TestSerialInterfaceName(t *testing.T) {
	const path = "USB#VID_1A86&PID_7523#8&39D912E3&0&3#{86e0d1e0-8089-11d0-9ce4-08003e301f73}"
	got, err := serialInterfaceName(`\\?\` + path)
	if err != nil || got != `\\.\`+path {
		t.Fatalf("serialInterfaceName returned %q, %v", got, err)
	}
	for _, alias := range []string{"COM3", `\\.\COM3`, `\\?\COM3`, ""} {
		if got, err := serialInterfaceName(alias); err == nil {
			t.Errorf("accepted COM alias %q as interface %q", alias, got)
		}
	}
}

// Exercise the native SetupAPI call even on machines with no panels. This
// reads device metadata only; COM handles and wake writes belong to sessions.
func TestEnumerateLightCandidates(t *testing.T) {
	ports, err := enumerateLightCandidates()
	if err != nil {
		t.Fatal(err)
	}
	for port, path := range ports {
		if _, err := serialInterfaceName(path); err != nil {
			t.Fatalf("%s: %v", port, err)
		}
		t.Logf("%s: %s", port, path)
	}
}
