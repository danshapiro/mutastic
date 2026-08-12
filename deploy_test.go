package main

import (
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
	// startup contract is exactly two hidden, asynchronous commands in this order.
	runTokenRE := regexp.MustCompile(`(?i)\.\s*Run\b`)
	runLineRE := regexp.MustCompile(`(?im)^\s*shell\s*\.\s*Run\s+"""([^"\r\n]+)""\s+([^"\r\n]+)"\s*,\s*([0-9]+)\s*,\s*(True|False)\s*$`)
	if tokens := runTokenRE.FindAll(raw, -1); len(tokens) != 2 {
		t.Fatalf("startup launcher contains %d .Run calls, want exactly 2", len(tokens))
	}
	runs := runLineRE.FindAllStringSubmatch(string(raw), -1)
	if len(runs) != 2 {
		t.Fatalf("parsed %d valid shell.Run lines, want exactly 2; launcher:\n%s", len(runs), raw)
	}

	const deployedExe = `C:\Users\dan\code\mutastic-deploy\mutastic.exe`
	wantArgs := [][]string{{"daemon"}, {"ui", "--no-open"}}
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
