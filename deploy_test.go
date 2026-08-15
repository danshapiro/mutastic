package main

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestMutasticAutostartVBSContract(t *testing.T) {
	raw, err := os.ReadFile("deploy/mutastic-daemon.vbs")
	if err != nil {
		t.Fatalf("read Mutastic startup launcher: %v", err)
	}

	setShellRE := regexp.MustCompile(`(?im)^\s*Set\s+shell\s*=\s*CreateObject\(\s*"WScript\.Shell"\s*\)\s*$`)
	if matches := setShellRE.FindAll(raw, -1); len(matches) != 1 {
		t.Fatalf("startup launcher initializes WScript.Shell %d times, want exactly 1", len(matches))
	}

	// Parse every shell.Run line rather than pinning indentation or spacing. The
	// startup contract is exactly three hidden, asynchronous commands in this order.
	runTokenRE := regexp.MustCompile(`(?i)\.\s*Run\b`)
	runLineRE := regexp.MustCompile(`(?im)^\s*shell\s*\.\s*Run\s+"""([^"\r\n]+)""\s+([^"\r\n]+)"\s*,\s*([0-9]+)\s*,\s*(True|False)\s*$`)
	if tokens := runTokenRE.FindAll(raw, -1); len(tokens) != 3 {
		t.Fatalf("startup launcher contains %d .Run calls, want exactly 3", len(tokens))
	}
	runs := runLineRE.FindAllStringSubmatch(string(raw), -1)
	if len(runs) != 3 {
		t.Fatalf("parsed %d valid shell.Run lines, want exactly 3; launcher:\n%s", len(runs), raw)
	}

	const deployedExe = `C:\Users\dan\code\mutastic-deploy\mutastic.exe`
	wantArgs := [][]string{{"daemon"}, {"ui", "--no-open"}, {"tray"}}
	for i, run := range runs {
		if !strings.EqualFold(strings.TrimSpace(run[1]), deployedExe) {
			t.Errorf("run %d executable = %q, want deployed %q", i+1, run[1], deployedExe)
		}
		gotArgs := strings.Fields(run[2])
		if strings.Join(gotArgs, "\x00") != strings.Join(wantArgs[i], "\x00") {
			t.Errorf("run %d arguments = %q, want %q", i+1, gotArgs, wantArgs[i])
		}
		if run[3] != "0" {
			t.Errorf("run %d window style = %q, want 0 (hidden)", i+1, run[3])
		}
		if !strings.EqualFold(run[4], "False") {
			t.Errorf("run %d wait flag = %q, want False (asynchronous)", i+1, run[4])
		}
		if len(gotArgs) == 1 && strings.EqualFold(gotArgs[0], "ui") {
			t.Errorf("run %d starts bare 'mutastic.exe ui'; login startup must use ui --no-open", i+1)
		}
	}
}

func TestMuteAllMeetingsHotkeyContract(t *testing.T) {
	raw, err := os.ReadFile("ahk/MuteAllMeetings.ahk")
	if err != nil {
		t.Fatalf("read MuteAllMeetings AHK script: %v", err)
	}
	if !bytes.HasPrefix(raw, []byte{0xef, 0xbb, 0xbf}) {
		t.Fatal("MuteAllMeetings.ahk must retain its UTF-8 BOM")
	}
	if !bytes.Contains(raw, []byte("\r\n")) {
		t.Fatal("MuteAllMeetings.ahk must use CRLF line endings")
	}
	withoutCRLF := bytes.ReplaceAll(raw, []byte("\r\n"), nil)
	if bytes.Contains(withoutCRLF, []byte("\n")) {
		t.Fatal("MuteAllMeetings.ahk contains a bare LF")
	}

	lines := normalizedAHKCodeLines(string(raw))
	if countSourceLine(lines, "*f13::return") != 1 {
		t.Fatalf("active F13 handler must be exactly '*F13::return'; lines=%v", lines)
	}
	if countSourceLine(lines, "*f14::return") != 1 {
		t.Fatalf("active F14 handler must be exactly '*F14::return'; lines=%v", lines)
	}
	for _, line := range lines {
		if strings.Contains(line, "f13::") && line != "*f13::return" {
			t.Fatalf("unexpected active F13 handler: %q", line)
		}
		if strings.Contains(line, "f14::") && line != "*f14::return" {
			t.Fatalf("unexpected active F14 handler: %q", line)
		}
		if strings.Contains(line, "f15::") {
			t.Fatalf("F15 must remain unbound, found active declaration: %q", line)
		}
		if strings.Contains(line, "~f13") || strings.Contains(line, "~f14") {
			t.Fatalf("F13/F14 must be consumed, not pass through: %q", line)
		}
	}
	code := strings.Join(lines, "\n")
	if strings.Contains(code, "light toggle") {
		t.Fatal("active AHK must not invoke mutastic light toggle")
	}

	wantF24 := []string{"*f24::", "toggleallmeetings()", "return"}
	matches := 0
	for i := 0; i+len(wantF24) <= len(lines); i++ {
		if strings.Join(lines[i:i+len(wantF24)], "\x00") == strings.Join(wantF24, "\x00") {
			matches++
		}
	}
	if matches != 1 {
		t.Fatalf("active F24 path must contain exactly one three-line sequence %q; found %d", wantF24, matches)
	}

	// MuteAllMeetings must not own a tray icon: mic state and quit live on the
	// mutastic tray icon now; this script is hotkeys only.
	notray := 0
	for _, line := range lines {
		if line == "#notrayicon" {
			notray++
		}
		if strings.HasPrefix(line, "menu, tray,") {
			t.Fatalf("MuteAllMeetings must not set tray icon/tip; found %q", line)
		}
	}
	if notray != 1 {
		t.Fatalf("expected exactly one #NoTrayIcon directive, found %d", notray)
	}

	docRaw, err := os.ReadFile("docs/pedal-and-mute.md")
	if err != nil {
		t.Fatalf("read pedal documentation: %v", err)
	}
	docLines := normalizedSourceLines(string(docRaw))
	findUniqueSourceLine(t, docLines,
		"| left | `f13` | disabled 2026-08-12; consumed no-op (light control remains in browser ui, stream deck, and `mutastic light ...`) |",
	)
	findUniqueSourceLine(t, docLines,
		"| center | `f14` | disabled 2026-08-09; consumed no-op because of accidental presses |",
	)
	findUniqueSourceLine(t, docLines,
		"| right | `f15` | winpepper push-to-talk hold hotkey |",
	)
}

func normalizedAHKCodeLines(source string) []string {
	source = strings.TrimPrefix(source, "\ufeff")
	source = strings.ReplaceAll(source, "\r\n", "\n")
	var lines []string
	for _, line := range strings.Split(source, "\n") {
		if comment := strings.IndexByte(line, ';'); comment >= 0 {
			line = line[:comment]
		}
		line = strings.ToLower(strings.Join(strings.Fields(line), " "))
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func countSourceLine(lines []string, want string) int {
	count := 0
	for _, line := range lines {
		if line == want {
			count++
		}
	}
	return count
}

func TestDeployCMDMutasticAutostartContract(t *testing.T) {
	raw, err := os.ReadFile("deploy/deploy.cmd")
	if err != nil {
		t.Fatalf("read deploy script: %v", err)
	}
	lines := normalizedSourceLines(string(raw))

	copyLine := findUniqueSourceLine(t, lines,
		`copy /y "%src%\deploy\mutastic-daemon.vbs" "%dest%\mutastic-daemon.vbs"`,
		`|| goto :fail`,
	)
	shortcutLine := findUniqueSourceLine(t, lines,
		`$ws.createshortcut('%startup%\mutastic daemon.lnk')`,
		`$s.targetpath = 'c:\windows\system32\wscript.exe'`,
		`$s.arguments = '"%dest%\mutastic-daemon.vbs"'`,
		`$s.workingdirectory = '%dest%'`,
		`$s.save()`,
		`|| goto :fail`,
	)
	approvalLine := findUniqueSourceLine(t, lines,
		`hkcu:\software\microsoft\windows\currentversion\explorer\startupapproved\startupfolder`,
		`if (test-path -literalpath $key)`,
		`remove-itemproperty -literalpath $key -name 'mutastic daemon.lnk'`,
		`(get-item -literalpath $key -erroraction stop).property -contains 'mutastic daemon.lnk'`,
		`exit 1`,
		`|| goto :fail`,
	)
	relaunchLine := findUniqueSourceLine(t, lines,
		`start "" wscript.exe "%dest%\mutastic-daemon.vbs"`,
	)

	if !(copyLine < shortcutLine && shortcutLine < approvalLine && approvalLine < relaunchLine) {
		t.Fatalf(
			"autostart operations are out of order: copy=%d shortcut=%d StartupApproved=%d relaunch=%d",
			copyLine, shortcutLine, approvalLine, relaunchLine,
		)
	}
}

func normalizedSourceLines(source string) []string {
	source = strings.ReplaceAll(source, "\r\n", "\n")
	lines := strings.Split(source, "\n")
	for i := range lines {
		lines[i] = strings.ToLower(strings.Join(strings.Fields(lines[i]), " "))
	}
	return lines
}

func findUniqueSourceLine(t *testing.T, lines []string, fragments ...string) int {
	t.Helper()
	found := -1
	for i, line := range lines {
		matches := true
		for _, fragment := range fragments {
			if !strings.Contains(line, strings.ToLower(fragment)) {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}
		if found != -1 {
			t.Fatalf("source contract matched more than one line for %q", fragments)
		}
		found = i
	}
	if found == -1 {
		t.Fatalf("source contract found no line containing every fragment %q", fragments)
	}
	return found
}
