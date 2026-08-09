// mutastic controls the Blue Yeti X hardware mute and the NEEWER PL81 PRO
// streaming light.
//
//	mutastic daemon                     resident: HID + serial sessions + UDP server
//	mutastic toggle|mute|unmute|status  one-shot client: mic hardware mute
//	mutastic light <subcommand...>      one-shot client: light control
package main

import (
	"context"
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

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	if os.Args[1] == "daemon" {
		os.Exit(runDaemon())
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
	case args[0] == "light" || strings.HasPrefix(args[0], "light@"):
		if len(args) < 2 {
			return "", 0, false
		}
		return strings.Join(args, " "), 2 * time.Second, true
	}
	return "", 0, false
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: mutastic daemon | toggle | mute | unmute | status")
	fmt.Fprintln(os.Stderr, "       mutastic light toggle|on|off|status|list  (bare light commands act on ALL lights)")
	fmt.Fprintln(os.Stderr, "       mutastic light brightness <0-100> | temp <2900-7000> | preset <cold|sunlight|afternoon|sunset|candle>")
	fmt.Fprintln(os.Stderr, "       mutastic light name <COMx> <name> | unname <name|COMx>")
	fmt.Fprintln(os.Stderr, "       mutastic light@<name|COMx> <command>  (one light)")
}

// runClient sends one UDP command to the daemon and prints the reply.
// Exit codes: 0 = ok, 1 = "error:" reply from the daemon, 2 = no daemon.
func runClient(cmd, addr string, timeout time.Duration, out io.Writer) int {
	conn, err := net.Dial("udp", addr)
	if err != nil {
		fmt.Fprintln(out, "error: no daemon reachable:", err)
		return 2
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write([]byte(cmd)); err != nil {
		fmt.Fprintln(out, "error: no daemon reachable:", err)
		return 2
	}
	buf := make([]byte, 2048) // multi-light list/fan-out replies exceed 256 bytes
	n, err := conn.Read(buf)
	if err != nil {
		fmt.Fprintln(out, "error: no daemon reachable")
		return 2
	}
	reply := strings.TrimSpace(string(buf[:n]))
	fmt.Fprintln(out, reply)
	if strings.HasPrefix(reply, "error:") {
		return 1
	}
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
	logger := log.New(io.MultiWriter(os.Stderr, logw), "", log.LstdFlags)
	logger.Printf("mutastic daemon starting (log: %s)", logPath)

	pc, err := net.ListenPacket("udp", udpAddr)
	if err != nil {
		// Port already bound: another daemon instance is running.
		logger.Printf("cannot bind %s (daemon already running?): %v", udpAddr, err)
		return 1
	}
	open := func() (daemon.Device, error) { return openYetiX(logger) }
	ctx := context.Background()
	lm := light.NewManager(logger, lightStatePath())
	lm.Present = pl81Present
	go lm.Run(ctx, func() (light.Port, error) { return openPL81(logger) })
	daemon.Run(ctx, open, lm, pc, logger)
	return 0
}

// openLogFile opens %LOCALAPPDATA%\mutastic\mutastic.log (os.UserCacheDir
// is %LOCALAPPDATA% on Windows), rotating to .old above 5 MB.
func openLogFile() (io.WriteCloser, string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return nil, "", err
	}
	logDir := filepath.Join(dir, "mutastic")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, "", err
	}
	path := filepath.Join(logDir, "mutastic.log")
	if fi, err := os.Stat(path); err == nil && fi.Size() > 5<<20 {
		os.Rename(path, path+".old")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, "", err
	}
	return f, path, nil
}

// lightStatePath returns %LOCALAPPDATA%\mutastic\light-state.json (the same
// directory as mutastic.log). An empty string disables persistence rather
// than failing the daemon.
func lightStatePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "mutastic", "light-state.json")
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }
