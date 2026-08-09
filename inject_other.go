//go:build !windows

package main

import "mutastic/internal/daemon"

// newKeyInjector returns the platform key injector. Non-Windows builds
// have none: the daemon treats a nil KeyInjector as "not wired" and
// skips the mic-button hook entirely (same spirit as openYetiX's stub).
func newKeyInjector() daemon.KeyInjector { return nil }
