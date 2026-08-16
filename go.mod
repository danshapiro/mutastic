module mutastic

go 1.26.3

require (
	github.com/energye/systray v1.0.3
	github.com/gorilla/websocket v1.5.3
	github.com/sstallion/go-hid v0.15.0
	go.bug.st/serial v1.8.0
)

require (
	github.com/godbus/dbus/v5 v5.1.0 // indirect
	golang.org/x/sys v0.43.0 // indirect
)

// R8-F4: the systray fork is vendored at third_party/systray with its
// native-dispatch callback bindings synchronized (third_party/systray/
// PATCHES.md); imports stay github.com/energye/systray.
replace github.com/energye/systray => ./third_party/systray
