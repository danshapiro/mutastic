//go:build !windows

package main

import (
	"fmt"
	"os"
)

// runTray stub: the notification-area icon is Windows-only (the production
// platform; development happens under WSL, where there is no tray).
func runTray() int {
	fmt.Fprintln(os.Stderr, "mutastic: tray mode is only supported on Windows")
	return 1
}
