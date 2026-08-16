//go:build !windows

package main

import (
	"fmt"
	"os"

	"github.com/energye/systray"
)

// runTray stub: the notification-area icon is Windows-only (the production
// platform; development happens under WSL, where there is no tray).
func runTray() int {
	fmt.Fprintln(os.Stderr, "mutastic: tray mode is only supported on Windows")
	return 1
}

// trayVerifySettingsItemTitle portable default (see the Windows arm in
// tray_windows.go): the stale-native-row dispatch race it guards exists
// only under Windows WM_COMMAND command-ID dispatch; other platforms
// dispatch Go-side, where a row's command slot and its title are updated
// on the same refresh goroutine with no yield between, so the slot loaded
// in the click closure can never disagree with what the row displays.
// Always true here.
func trayVerifySettingsItemTitle(item *systray.MenuItem, cmd string) bool {
	return true
}
