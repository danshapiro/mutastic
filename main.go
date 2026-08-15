// mutastic controls the Blue Yeti X hardware mute and the NEEWER PL81 PRO
// streaming light.
//
//	mutastic daemon                     resident: HID + serial sessions + UDP server
//	mutastic tray                       resident: system tray icon (mic status + quick actions)
//	mutastic toggle|mute|unmute|status  one-shot client: mic hardware mute
//	mutastic shutdown                   one-shot client: stop the daemon
//	mutastic light <subcommand...>      one-shot client: light control
//	mutastic ui                         local browser light control panel
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mutastic/internal/daemon"
	"mutastic/internal/light"
)

const udpAddr = "127.0.0.1:42814"

// lightClientTimeout is the client's read budget for light verbs. It MUST
// exceed the daemon's per-light stall bound (light.CallTimeout) with real
// headroom: the daemon's timer starts only after it receives the packet,
// so when a wedged light hits its deadline (the designed degraded mode),
// the reply - healthy lights' results plus the per-line "error: timeout" -
// lands just after light.CallTimeout. A client that gives up at or before
// that point prints "error: no daemon reachable" and masks partial success
// as total daemon failure. 3x leaves room for fan-out, UDP, and OS
// scheduling. Mic verbs keep their snappy 1s budget (see clientCommand).
const lightClientTimeout = 3 * light.CallTimeout

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	if os.Args[1] == "daemon" {
		if len(os.Args) > 2 && os.Args[2] == "--test-inject" {
			os.Exit(runTestInject(os.Stdout))
		}
		os.Exit(runDaemon())
	}
	// Stream Deck plugin mode. OpenDeck launches the binary from the
	// plugin directory with Elgato-style args and NO subcommand word
	// (mutastic.exe -port N -pluginUUID ... -registerEvent ... -info ...),
	// so a leading -port flag IS the plugin mode; the explicit
	// "deckplugin" word exists for manual/diagnostic launches.
	if os.Args[1] == "deckplugin" {
		os.Exit(runDeckPlugin(os.Args[2:]))
	}
	if os.Args[1] == "-port" {
		os.Exit(runDeckPlugin(os.Args[1:]))
	}
	// OBS snapshot mode: talks straight to obs-websocket, no daemon.
	if os.Args[1] == "obs" {
		os.Exit(runObs(os.Args[2:], os.Stdout, os.Stderr))
	}
	if os.Args[1] == "ui" {
		os.Exit(runUI(os.Args[2:], os.Stdout, os.Stderr))
	}
	// Resident system tray icon (Windows only; stub elsewhere).
	if os.Args[1] == "tray" {
		os.Exit(runTray())
	}
	cmd, timeout, ok := clientCommand(os.Args[1:])
	if !ok {
		usage()
		os.Exit(2)
	}
	os.Exit(runClient(cmd, udpAddr, timeout, os.Stdout))
}

// clientCommand maps argv (without the program name) to the UDP command
// string and timeout. ok=false means bad usage. Light commands are a dumb
// verbatim pass-through - the daemon owns the grammar.
func clientCommand(args []string) (cmd string, timeout time.Duration, ok bool) {
	if len(args) == 0 {
		return "", 0, false
	}
	switch {
	case args[0] == "toggle" || args[0] == "mute" || args[0] == "unmute" || args[0] == "status":
		return args[0], time.Second, true
	case args[0] == "shutdown":
		return args[0], lightClientTimeout, true
	case args[0] == "light" || strings.HasPrefix(args[0], "light@"):
		if len(args) < 2 {
			return "", 0, false
		}
		return strings.Join(args, " "), lightClientTimeout, true
	}
	return "", 0, false
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: mutastic daemon | toggle | mute | unmute | status | shutdown")
	fmt.Fprintln(os.Stderr, "       mutastic tray  (system tray icon: mic status + quick actions; Quit stops everything mutastic runs)")
	fmt.Fprintln(os.Stderr, "       mutastic deckplugin -port <N> -pluginUUID <uuid> -registerEvent <event> [-info <json>]  (OpenDeck plugin mode)")
	fmt.Fprintln(os.Stderr, "       mutastic light toggle|on|off|status|list  (bare light commands act on ALL lights)")
	fmt.Fprintln(os.Stderr, "       mutastic light brightness <0-100> | temp <2900-7000> | preset <cold|sunlight|afternoon|sunset|candle>")
	fmt.Fprintln(os.Stderr, "       mutastic light brightness-delta <-20..20> | temp-step-delta <-3..3>")
	fmt.Fprintln(os.Stderr, "       mutastic light name <COMx> <name> | unname <name|COMx>")
	fmt.Fprintln(os.Stderr, "       mutastic light@<name|COMx> <command>  (one light)")
	fmt.Fprintln(os.Stderr, "       mutastic obs snapshot --out <path> [--source <name>] | obs list-sources  (OBS still capture)")
	fmt.Fprintln(os.Stderr, "       mutastic ui [--port 42815] [--no-open]  (local browser light control panel)")
}

// runClient sends one UDP command to the daemon and prints the reply.
// Exit codes: 0 = ok, 1 = "error:" reply from the daemon, 2 = no daemon.
func runClient(cmd, addr string, timeout time.Duration, out io.Writer) int {
	reply, err := askDaemon(cmd, addr, timeout)
	switch {
	case errors.Is(err, errNoReply):
		fmt.Fprintln(out, "error: no daemon reachable")
		return 2
	case err != nil:
		fmt.Fprintln(out, "error: no daemon reachable:", err)
		return 2
	}
	fmt.Fprintln(out, reply)
	if strings.HasPrefix(reply, "error:") {
		return 1
	}
	return 0
}

// runTestInject exercises the SendInput plumbing once and exits: a hidden
// smoke command for live verification (`mutastic daemon --test-inject`).
// With the AHK script running, this fires the F24 meeting-app sweep
// exactly as a physical mic-button press would (harmless when no meeting
// windows are open: the sweep finds nothing).
func runTestInject(out io.Writer) int {
	inj := newKeyInjector()
	if inj == nil {
		fmt.Fprintln(out, "error: key injection is only supported on Windows")
		return 1
	}
	if err := inj.Inject(); err != nil {
		fmt.Fprintln(out, "error:", err)
		return 1
	}
	fmt.Fprintln(out, "injected F24")
	return 0
}

func runDaemon() int {
	hideConsoleIfOwned()

	logw, logPath, err := openLogFile()
	if err != nil {
		fmt.Fprintln(os.Stderr, "mutastic: cannot open log file:", err)
		logw = nopWriteCloser{}
	}
	defer logw.Close()
	// Logfile FIRST: io.MultiWriter aborts on the first destination error,
	// and stderr can die with a freed console on Windows - it must never be
	// able to drop logfile lines (the E2E log contract greps the file).
	logger := log.New(io.MultiWriter(logw, os.Stderr), "", log.LstdFlags)
	logger.Printf("mutastic daemon starting (log: %s)", logPath)

	pc, err := net.ListenPacket("udp", udpAddr)
	if err != nil {
		// Port already bound: another daemon instance is running.
		logger.Printf("cannot bind %s (daemon already running?): %v", udpAddr, err)
		return 1
	}
	open := func() (daemon.Device, error) { return openYetiX(logger) }
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	stateDir := lightStateDir()
	namesPath := ""
	if stateDir != "" {
		namesPath = filepath.Join(stateDir, "light-names.json")
	}
	reg := light.NewRegistry(namesPath)
	lights := light.NewMultiManager(logger, stateDir, reg, enumeratePL81Ports, openPL81Port)
	lightsDone := make(chan struct{})
	go func() {
		lights.Run(ctx)
		close(lightsDone)
	}()
	daemon.Run(ctx, open, lights, newKeyInjector(), stop, pc, logger)
	// Join the light manager before exiting: its ctx-done teardown drains
	// each serial session (bounded internally by drainTimeout per light),
	// and process exit must not cut that off mid-write.
	<-lightsDone
	logger.Printf("mutastic daemon stopped")
	return 0
}

// openNamedLogFile opens %LOCALAPPDATA%\mutastic\<name> (os.UserCacheDir
// is %LOCALAPPDATA% on Windows), rotating to <name>.old above 5 MB.
// The daemon and the deckplugin use SEPARATE files: two processes racing
// the rename-rotation on one file would be a real hazard.
func openNamedLogFile(name string) (io.WriteCloser, string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return nil, "", err
	}
	logDir := filepath.Join(dir, "mutastic")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, "", err
	}
	path := filepath.Join(logDir, name)
	if fi, err := os.Stat(path); err == nil && fi.Size() > 5<<20 {
		os.Rename(path, path+".old")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, "", err
	}
	return f, path, nil
}

// openLogFile opens the daemon's log (%LOCALAPPDATA%\mutastic\mutastic.log).
func openLogFile() (io.WriteCloser, string, error) {
	return openNamedLogFile("mutastic.log")
}

// lightStateDir returns %LOCALAPPDATA%\mutastic (the same directory as
// mutastic.log); per-light state files and the name registry live here.
// An empty string disables persistence rather than failing the daemon.
func lightStateDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "mutastic")
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }
