//go:build !windows

package main

import (
	"errors"

	"mutastic/internal/light"
)

// The daemon only supports Windows; these stubs keep cross-platform
// builds and tests compiling.
func enumerateLightCandidates() (map[string]string, error) {
	return nil, errors.New("the mutastic daemon only supports Windows")
}

func openPL81Port(_ string) (light.Port, error) {
	return nil, errors.New("the mutastic daemon only supports Windows")
}
