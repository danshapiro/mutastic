// mutastic controls the Blue Yeti X hardware mute.
//
//	mutastic daemon                     resident: HID session + UDP server
//	mutastic toggle|mute|unmute|status  one-shot client for the daemon
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
)

const udpAddr = "127.0.0.1:42814"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "daemon":
		os.Exit(runDaemon())
	case "toggle", "mute", "unmute", "status":
		os.Exit(runClient(os.Args[1], udpAddr, time.Second, os.Stdout))
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: mutastic daemon | toggle | mute | unmute | status")
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
	buf := make([]byte, 256)
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
	daemon.Run(context.Background(), open, pc, logger)
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

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }
