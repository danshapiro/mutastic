//go:build !windows

package main

import (
	"errors"
	"log"

	"mutastic/internal/daemon"
)

func openYetiX(_ *log.Logger) (daemon.Device, error) {
	return nil, errors.New("the mutastic daemon only supports Windows")
}

func hideConsoleIfOwned() {}
