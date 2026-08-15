// deckplugin.go wires the OpenDeck (Elgato Stream Deck SDK) plugin mode
// into the mutastic binary. OpenDeck spawns this exe from
// %APPDATA%\opendeck\plugins\com.danshapiro.mutastic.sdPlugin\ with
//
//	mutastic.exe -port <N> -pluginUUID <dir name> -registerEvent registerPlugin -info <json>
//
// (working directory = the plugin dir, CREATE_NO_WINDOW, stdout/stderr
// redirected to OpenDeck's per-plugin log). main() detects either the
// explicit "deckplugin" subcommand or that leading -port flag. The
// platform-free protocol + state machine live in internal/deckplugin;
// this file supplies the real WebSocket, UDP client, F24 injector, and
// log file.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"mutastic/internal/deckplugin"
	"mutastic/internal/light"
)

// errNoReply distinguishes "daemon reached but no reply arrived" from
// dial/write failures, so runClient can preserve its exact historical
// output for each case.
var errNoReply = errors.New("no reply from daemon")

// askDaemon sends one UDP command to the daemon and returns the trimmed
// reply. It is the reply-returning core of runClient (which prints);
// both share the daemon's plain-text protocol on udpAddr.
func askDaemon(cmd, addr string, timeout time.Duration) (string, error) {
	conn, err := net.Dial("udp", addr)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write([]byte(cmd)); err != nil {
		return "", err
	}
	buf := make([]byte, 8192) // a full saved-settings store's list fits: 100 names x <=42 bytes <= ~4.3 KB
	n, err := conn.Read(buf)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errNoReply, err)
	}
	return strings.TrimSpace(string(buf[:n])), nil
}

// wsConn adapts gorilla/websocket to deckplugin.Conn.
type wsConn struct{ c *websocket.Conn }

func (w wsConn) ReadMessage() ([]byte, error) {
	_, data, err := w.c.ReadMessage()
	return data, err
}

func (w wsConn) WriteMessage(data []byte) error {
	return w.c.WriteMessage(websocket.TextMessage, data)
}

// lightPluginTimeout is the plugin's UDP read budget for light verbs.
// It must exceed the daemon's per-light stall bound (light.CallTimeout):
// a wedged light's degraded reply lands just after that bound. Unlike
// the CLI's 6s lightClientTimeout, the plugin adds only 500ms of
// headroom because this call blocks the plugin's single event-loop
// goroutine — the budget also caps how long a wedged light can stall
// mute-key handling. A missed reply just holds the icon one tick.
const lightPluginTimeout = light.CallTimeout + 500*time.Millisecond

// commandTimeout picks the UDP read budget for one plugin->daemon
// command. The light-prefix rule mirrors the daemon's routing in
// daemon.HandleCommand: "light" + end-of-string, space, or '@'.
func commandTimeout(cmd string) time.Duration {
	if rest, ok := strings.CutPrefix(cmd, "light"); ok && (rest == "" || rest[0] == ' ' || rest[0] == '@') {
		return lightPluginTimeout
	}
	return time.Second
}

// udpDaemonClient implements deckplugin.DaemonClient: one UDP round
// trip per call with a per-verb timeout (see commandTimeout).
type udpDaemonClient struct {
	addr string
}

func (u udpDaemonClient) Command(cmd string) (string, error) {
	return askDaemon(cmd, u.addr, commandTimeout(cmd))
}

// runDeckPlugin is the plugin-mode entry point. args excludes the
// program name and the optional "deckplugin" word. Exit codes: 0 clean
// shutdown (OpenDeck closed the socket), 1 runtime failure, 2 bad usage.
func runDeckPlugin(args []string) int {
	cfg, err := deckplugin.ParseArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "deckplugin:", err)
		return 2
	}

	logw, logPath, err := openNamedLogFile("deckplugin.log")
	if err != nil {
		fmt.Fprintln(os.Stderr, "deckplugin: cannot open log file:", err)
		logw = nopWriteCloser{}
	}
	defer logw.Close()
	// Logfile FIRST: io.MultiWriter aborts on the first destination
	// error, and stderr here is OpenDeck's redirected pipe, which dies
	// with OpenDeck. Same invariant as the daemon logger in main.go.
	logger := log.New(io.MultiWriter(logw, os.Stderr), "", log.LstdFlags)
	logger.Printf("deckplugin starting: port=%d uuid=%s (log: %s)", cfg.Port, cfg.PluginUUID, logPath)

	// OpenDeck binds its WebSocket server before spawning plugins, but a
	// short retry makes startup races and slow boots harmless.
	url := fmt.Sprintf("ws://127.0.0.1:%d", cfg.Port)
	var ws *websocket.Conn
	for attempt := 1; ; attempt++ {
		ws, _, err = websocket.DefaultDialer.Dial(url, nil)
		if err == nil {
			break
		}
		if attempt >= 5 {
			logger.Printf("dial %s failed after %d attempts: %v", url, attempt, err)
			return 1
		}
		logger.Printf("dial %s (attempt %d): %v -- retrying in 1s", url, attempt, err)
		time.Sleep(time.Second)
	}
	defer ws.Close()

	var inject deckplugin.Injector
	if ki := newKeyInjector(); ki != nil {
		inject = ki // nil on non-Windows: keyDown still toggles the daemon, skips F24
	}
	p := deckplugin.New(wsConn{ws}, udpDaemonClient{udpAddr}, inject, logger)
	if err := p.Run(context.Background(), cfg.RegisterEvent, cfg.PluginUUID); err != nil {
		logger.Printf("deckplugin exiting: %v", err)
		return 1
	}
	logger.Printf("deckplugin exiting (socket closed)")
	return 0
}
