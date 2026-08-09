//go:build !windows

package main

import (
	"errors"
	"log"

	"mutastic/internal/light"
)

func openPL81(_ *log.Logger) (light.Port, error) {
	return nil, errors.New("the mutastic daemon only supports Windows")
}

// pl81Present is never consulted off-Windows (no session can ever start).
func pl81Present() bool { return false }
