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
