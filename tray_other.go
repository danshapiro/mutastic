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

// trayVerifyArmed portable default (see the Windows arm in
// tray_windows.go): the R6-F3 hazard it closes is a NATIVE paint failure
// or a mid-paint click on Windows (the systray fork's SetTitle/
// Enable/Disable report failure only through its own log; the visible
// row would then diverge from the armed premise). On other platforms the
// title/enabled writes and the premise snapshot are plain Go state
// updated on the same refresh goroutine, so the painted row cannot
// diverge from the snapshot. Always true here; the library is trusted to
// log native failures (the documented portable residual).
func trayVerifyArmed(item *systray.MenuItem, snap trayMuteSnapshot) bool {
	return true
}
