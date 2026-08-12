package main

import (
	"bytes"
	"net"
	"strconv"
	"strings"
	"testing"
)

func TestRunObsUsageErrors(t *testing.T) {
	cases := map[string][]string{
		"no action":      {},
		"unknown action": {"bogus"},
		"missing --out":  {"snapshot"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			var out, errw bytes.Buffer
			if code := runObs(args, &out, &errw); code != 2 {
				t.Fatalf("exit code = %d, want 2 (stderr: %s)", code, errw.String())
			}
		})
	}
}

func TestRunObsConnectionRefused(t *testing.T) {
	// Grab a port that is definitely closed: listen, note it, close it.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	var out, errw bytes.Buffer
	code := runObs([]string{"list-sources", "--host", "127.0.0.1", "--port", strconv.Itoa(port)}, &out, &errw)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (stderr: %s)", code, errw.String())
	}
	if !strings.Contains(errw.String(), "connect ws://127.0.0.1:"+strconv.Itoa(port)) {
		t.Fatalf("stderr = %q, want connect error naming the endpoint", errw.String())
	}
}
