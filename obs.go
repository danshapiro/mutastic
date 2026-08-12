// obs.go wires the `mutastic obs` subcommand into the binary: it owns the
// flags, the real gorilla/websocket dial, and the output file, while the
// protocol lives in internal/obs behind the obs.Conn interface (the same
// split as deckplugin.go / internal/deckplugin). Unlike every other
// subcommand this one does NOT go through the daemon - it talks straight
// to OBS Studio's obs-websocket server.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/gorilla/websocket"

	"mutastic/internal/obs"
)

// obsBudget caps the whole websocket conversation (handshake + requests).
// A screenshot is a couple of small frames plus one image frame on
// loopback; 15s is generous while still failing fast when OBS is wedged.
const obsBudget = 15 * time.Second

// runObs is the `mutastic obs <action>` entry point. args excludes the
// program name and the "obs" word. Exit codes: 0 ok, 1 failure, 2 bad
// usage (the repo-wide convention).
func runObs(args []string, out, errw io.Writer) int {
	if len(args) == 0 {
		obsUsage(errw)
		return 2
	}
	action, rest := args[0], args[1:]
	switch action {
	case "snapshot", "list-sources":
	default:
		fmt.Fprintf(errw, "obs: unknown action %q\n", action)
		obsUsage(errw)
		return 2
	}

	fs := flag.NewFlagSet("obs "+action, flag.ContinueOnError)
	fs.SetOutput(errw)
	host := fs.String("host", "127.0.0.1", "obs-websocket host")
	port := fs.Int("port", 4455, "obs-websocket port")
	password := fs.String("password", "", "obs-websocket password (or env OBS_WS_PASSWORD)")
	outPath := fs.String("out", "", "output image file (required for snapshot)")
	source := fs.String("source", "", "source/scene to capture (default: current program scene)")
	width := fs.Int("width", 1280, "requested image width")
	height := fs.Int("height", 720, "requested image height")
	format := fs.String("format", "", "image format: jpg or png (default: from --out extension, else jpg)")
	if err := fs.Parse(rest); err != nil {
		return 2 // the flag package already printed the problem to errw
	}
	if action == "snapshot" && *outPath == "" {
		fmt.Fprintln(errw, "obs snapshot: --out <path> is required")
		return 2
	}
	if *password == "" {
		*password = os.Getenv("OBS_WS_PASSWORD")
	}

	client, closeConn, err := dialOBS(*host, *port, *password)
	if err != nil {
		fmt.Fprintln(errw, "obs:", err)
		if errors.Is(err, obs.ErrAuthRequired) {
			fmt.Fprintln(errw, "obs: pass --password or set OBS_WS_PASSWORD")
		}
		return 1
	}
	defer closeConn()

	switch action {
	case "snapshot":
		err = obsSnapshot(client, *outPath, *source, *format, *width, *height, out)
	case "list-sources":
		err = obsListSources(client, out)
	}
	if err != nil {
		fmt.Fprintln(errw, "obs:", err)
		return 1
	}
	return 0
}

// dialOBS connects to ws://host:port and runs the v5 handshake. The
// returned close func tears down the socket; the whole conversation
// shares one obsBudget read deadline set here.
func dialOBS(host string, port int, password string) (*obs.Client, func(), error) {
	url := fmt.Sprintf("ws://%s:%d", host, port)
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	ws, _, err := dialer.Dial(url, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("connect %s: %w (is OBS running with obs-websocket enabled?)", url, err)
	}
	ws.SetReadDeadline(time.Now().Add(obsBudget))
	client, err := obs.Handshake(wsConn{ws}, password)
	if err != nil {
		ws.Close()
		return nil, nil, err
	}
	return client, func() { ws.Close() }, nil
}

// obsSnapshot captures one frame and writes it to outPath, printing the
// absolute path and byte size on success.
func obsSnapshot(client *obs.Client, outPath, source, format string, width, height int, out io.Writer) error {
	if source == "" {
		scene, err := client.CurrentProgramScene()
		if err != nil {
			return err
		}
		source = scene
	}
	if format == "" {
		format = obs.FormatFromPath(outPath)
	}
	img, err := client.Snapshot(obs.SnapshotRequest{Source: source, Format: format, Width: width, Height: height})
	if err != nil {
		return err
	}
	if err := os.WriteFile(outPath, img, 0o644); err != nil {
		return err
	}
	abs, err := filepath.Abs(outPath)
	if err != nil {
		abs = outPath // still report success; the write itself worked
	}
	fmt.Fprintf(out, "%s (%d bytes, source %q)\n", abs, len(img), source)
	return nil
}

// obsListSources prints every name GetSourceScreenshot will accept:
// scenes first, then inputs.
func obsListSources(client *obs.Client, out io.Writer) error {
	scenes, inputs, err := client.Sources()
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "scenes:")
	for _, s := range scenes {
		fmt.Fprintf(out, "  %s\n", s)
	}
	fmt.Fprintln(out, "inputs:")
	for _, i := range inputs {
		fmt.Fprintf(out, "  %s\n", i)
	}
	return nil
}

func obsUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: mutastic obs snapshot --out <path> [--source <name>] [--width N] [--height N] [--format jpg|png]")
	fmt.Fprintln(w, "       mutastic obs list-sources")
	fmt.Fprintln(w, "       common flags: --host <addr> --port <N> --password <pw>  (or env OBS_WS_PASSWORD)")
}
