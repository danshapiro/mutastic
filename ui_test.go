package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

const testLightStatus = "COM12 desk-center: on 55% 3583K\n" +
	"COM4 desk-right: off\n" +
	"COM6 desk-left: on 99% 3583K\n" +
	"COM5: unknown\n"

func TestParseUILightStatusStatesNamesAndSorting(t *testing.T) {
	lights, details := parseUILightStatus(testLightStatus)
	if len(details) != 0 {
		t.Fatalf("parse details = %+v, want none", details)
	}
	if len(lights) != 4 {
		t.Fatalf("got %d lights, want 4", len(lights))
	}
	gotPorts := make([]string, len(lights))
	for i, light := range lights {
		gotPorts[i] = light.Port
	}
	wantPorts := []string{"COM6", "COM4", "COM12", "COM5"}
	if !reflect.DeepEqual(gotPorts, wantPorts) {
		t.Fatalf("sorted ports = %v, want %v", gotPorts, wantPorts)
	}
	gotNames := make([]string, len(lights))
	for i, light := range lights {
		gotNames[i] = light.Name
	}
	wantNames := []string{"desk-left", "desk-right", "desk-center", ""}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("sorted names = %v, want %v", gotNames, wantNames)
	}

	byPort := make(map[string]uiLight, len(lights))
	for _, light := range lights {
		byPort[light.Port] = light
	}
	if byPort["COM6"].State != "on" || byPort["COM6"].Brightness == nil || *byPort["COM6"].Brightness != 99 || byPort["COM6"].Temp == nil || *byPort["COM6"].Temp != 3583 || !byPort["COM6"].Connected {
		t.Fatalf("on light parsed incorrectly: %+v", byPort["COM6"])
	}
	if byPort["COM4"].State != "off" || byPort["COM4"].Connected != true {
		t.Fatalf("off light parsed incorrectly: %+v", byPort["COM4"])
	}
	if byPort["COM5"].State != "unknown" || byPort["COM5"].Name != "" {
		t.Fatalf("unknown light parsed incorrectly: %+v", byPort["COM5"])
	}
}

func TestUIAPILightStatusReturnsSpatialOrder(t *testing.T) {
	var commands []string
	server := newUIServer(42815, newTestDaemonDispatcher(func(command string) (string, error) {
		commands = append(commands, command)
		return testLightStatus, nil
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/lights", nil)
	req.Host = "127.0.0.1:42815"
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body.String())
	}

	var response uiResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(response.Lights))
	for i, light := range response.Lights {
		got[i] = light.Name
	}
	want := []string{"desk-left", "desk-right", "desk-center", ""}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("API light names = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(commands, []string{"light status"}) {
		t.Fatalf("daemon commands = %v, want light status", commands)
	}
}

func TestSortUILightsOrdersExtraNamesAndUnnamedPorts(t *testing.T) {
	reply := strings.Join([]string{
		"COM30 zeta: off",
		"COM12 desk-center: off",
		"COM99: unknown",
		"COM7 desk-left: off",
		"COM4 desk-right: off",
		"COM2 alpha: off",
		"COM10: unknown",
	}, "\n")
	lights, details := parseUILightStatus(reply)
	if len(details) != 0 {
		t.Fatalf("parse details = %+v, want none", details)
	}

	got := make([]string, len(lights))
	for i, light := range lights {
		got[i] = light.Name + "@" + light.Port
	}
	want := []string{
		"desk-left@COM7",
		"desk-right@COM4",
		"desk-center@COM12",
		"alpha@COM2",
		"zeta@COM30",
		"@COM10",
		"@COM99",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sorted lights = %v, want %v", got, want)
	}
}

func TestLightMutationQueueNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatalf("node is required for the executable light mutation queue tests: %v", err)
	}
	script := filepath.Join("internal", "lightui", "mutation_queue_test.js")
	output, err := exec.Command(node, script).CombinedOutput()
	if err != nil {
		t.Fatalf("node %s failed: %v\n%s", script, err, output)
	}
}

func TestParseUILightStatusPreservesPartialError(t *testing.T) {
	lights, details := parseUILightStatus("COM4 desk: on 52% 2900K\nCOM12 right: error: timeout\nCOM7 left: off")
	if len(lights) != 3 || len(details) != 1 {
		t.Fatalf("lights/details = %+v / %+v, want 3 lights and 1 detail", lights, details)
	}
	byPort := make(map[string]uiLight, len(lights))
	for _, light := range lights {
		byPort[light.Port] = light
	}
	if byPort["COM12"].State != "error" || byPort["COM12"].Error != "error: timeout" {
		t.Fatalf("error status = %+v", byPort["COM12"])
	}
	if details[0].Target != "COM12" || details[0].Error != "error: timeout" {
		t.Fatalf("error detail = %+v", details[0])
	}
}

func TestBuildLightCommandValidation(t *testing.T) {
	cases := []struct {
		name string
		req  uiActionRequest
		want string
		bad  bool
	}{
		{name: "named brightness", req: uiActionRequest{Target: "Desk-Left", Action: "brightness", Value: intPointer(52)}, want: "light@desk-left brightness 52"},
		{name: "multi digit port temp", req: uiActionRequest{Target: "com12", Action: "temp", Value: intPointer(7000)}, want: "light@COM12 temp 7000"},
		{name: "toggle", req: uiActionRequest{Target: "desk-right", Action: "toggle"}, want: "light@desk-right toggle"},
		{name: "bad target", req: uiActionRequest{Target: "COM 4", Action: "on"}, bad: true},
		{name: "bad brightness", req: uiActionRequest{Target: "desk", Action: "brightness", Value: intPointer(101)}, bad: true},
		{name: "bad temperature step", req: uiActionRequest{Target: "desk", Action: "temp", Value: intPointer(3500)}, bad: true},
		{name: "missing value", req: uiActionRequest{Target: "desk", Action: "brightness"}, bad: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildLightCommand(tc.req)
			if tc.bad {
				if err == nil {
					t.Fatalf("buildLightCommand() = %q, want error", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("buildLightCommand() = %q, %v, want %q", got, err, tc.want)
			}
		})
	}
}

func TestGroupDeltaUsesOneAtomicDaemonCommand(t *testing.T) {
	for _, testCase := range []struct {
		action string
		value  int
		want   string
	}{
		{action: "brightness-delta", value: -20, want: "light brightness-delta -20"},
		{action: "temp-step-delta", value: 3, want: "light temp-step-delta 3"},
	} {
		t.Run(testCase.action, func(t *testing.T) {
			var commands []string
			plan, err := buildGroupPlan(uiActionRequest{Action: testCase.action, Value: intPointer(testCase.value)})
			if err != nil {
				t.Fatal(err)
			}
			details, err := plan(func(command string) (string, error) {
				commands = append(commands, command)
				return "COM4: on 25% 3583K", nil
			})
			if err != nil || len(details) != 0 {
				t.Fatalf("plan = details %v, err %v", details, err)
			}
			if !reflect.DeepEqual(commands, []string{testCase.want}) {
				t.Fatalf("daemon commands = %v, want [%q]", commands, testCase.want)
			}
		})
	}
}

func TestGroupDeltaPreservesPartialErrorsFromDaemon(t *testing.T) {
	plan, err := buildGroupPlan(uiActionRequest{Action: "brightness-delta", Value: intPointer(5)})
	if err != nil {
		t.Fatal(err)
	}
	details, err := plan(func(command string) (string, error) {
		if command != "light brightness-delta 5" {
			t.Fatalf("command = %q, want atomic brightness delta", command)
		}
		return "COM4: error: timeout\nCOM7: on 45% 3583K", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []uiDetail{{Target: "COM4", Error: "error: timeout"}}
	if !reflect.DeepEqual(details, want) {
		t.Fatalf("details = %+v, want %+v", details, want)
	}
}

func TestUIDispatcherSerializesRoundTrips(t *testing.T) {
	var mu sync.Mutex
	active := 0
	maxActive := 0
	call := newTestDaemonDispatcher(func(command string) (string, error) {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		mu.Lock()
		active--
		mu.Unlock()
		return command, nil
	})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := call.sequence(func(roundTrip daemonCall) error {
				_, err := roundTrip("status")
				return err
			}); err != nil {
				t.Errorf("sequence() error = %v", err)
			}
		}()
	}
	wg.Wait()
	if maxActive != 1 {
		t.Fatalf("maximum concurrent daemon calls = %d, want 1", maxActive)
	}
}

func TestUIDispatcherGroupDeltaUsesOneCommandAndOneSnapshot(t *testing.T) {
	var (
		mu       sync.Mutex
		commands []string
	)
	call := func(command string) (string, error) {
		mu.Lock()
		commands = append(commands, command)
		mu.Unlock()
		switch command {
		case "light brightness-delta 5":
			return "COM4 left: on 25% 2900K", nil
		case "light status":
			return "COM4 left: on 25% 2900K", nil
		default:
			return "error: unexpected command", nil
		}
	}
	server := newUIServer(42815, newTestDaemonDispatcher(call))
	request := httptest.NewRequest(http.MethodPost, "/api/group", bytes.NewBufferString(`{"action":"brightness-delta","value":5}`))
	request.Host = "127.0.0.1:42815"
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("group delta status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	mu.Lock()
	gotCommands := append([]string(nil), commands...)
	mu.Unlock()
	wantCommands := []string{"light brightness-delta 5", "light status"}
	if !reflect.DeepEqual(gotCommands, wantCommands) {
		t.Fatalf("daemon commands = %v, want one atomic command plus one snapshot %v", gotCommands, wantCommands)
	}
}

func TestUIAPIHealthHostAndOriginSecurity(t *testing.T) {
	server := newUIServer(42815, newTestDaemonDispatcher(func(string) (string, error) {
		return "no lights known", nil
	}))

	request := func(method, path, host, origin string, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Host = host
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, req)
		return recorder
	}

	if got := request(http.MethodGet, "/api/health", "127.0.0.1:42815", "", "").Code; got != http.StatusOK {
		t.Fatalf("health status = %d, want 200", got)
	}
	if got := request(http.MethodGet, "/api/health", "192.168.1.1:42815", "", "").Code; got != http.StatusForbidden {
		t.Fatalf("foreign host status = %d, want 403", got)
	}
	if got := request(http.MethodPost, "/api/group", "localhost:42815", "http://evil.example", `{"action":"on"}`).Code; got != http.StatusForbidden {
		t.Fatalf("foreign origin status = %d, want 403", got)
	}
	if got := request(http.MethodPost, "/api/group", "localhost:42815", "http://localhost:42815", `{"action":"on"}`).Code; got != http.StatusOK {
		t.Fatalf("same-origin group status = %d, want 200", got)
	}
}

func TestUIHeadersRejectCrossSiteAndPreventFraming(t *testing.T) {
	server := newUIServer(42815, newTestDaemonDispatcher(func(string) (string, error) {
		return "no lights known", nil
	}))

	for _, path := range []string{"/", "/mutation_queue.js"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = "127.0.0.1:42815"
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", path, recorder.Code)
		}
		if got := recorder.Header().Get("Content-Security-Policy"); got != uiFrameAncestorsNone {
			t.Fatalf("%s CSP = %q, want %q", path, got, uiFrameAncestorsNone)
		}
		if got := recorder.Header().Get("X-Frame-Options"); got != "DENY" {
			t.Fatalf("%s X-Frame-Options = %q, want DENY", path, got)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.Host = "127.0.0.1:42815"
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("cross-site request status = %d, want 403", recorder.Code)
	}
}

func TestUIAPIMutationHappyAndPartialError(t *testing.T) {
	var mu sync.Mutex
	var commands []string
	call := func(command string) (string, error) {
		mu.Lock()
		commands = append(commands, command)
		mu.Unlock()
		switch command {
		case "light brightness 60":
			return "COM4 left: on 60% 3583K\nCOM7 right: on 60% 3583K", nil
		case "light status":
			return "COM4 left: on 60% 3583K\nCOM7 right: on 55% 3583K", nil
		default:
			return "error: unexpected command", nil
		}
	}
	server := newUIServer(42815, newTestDaemonDispatcher(call))

	req := httptest.NewRequest(http.MethodPost, "/api/group", bytes.NewBufferString(`{"action":"set-brightness","value":60}`))
	req.Host = "127.0.0.1:42815"
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("happy mutation status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	var response uiResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Lights) != 2 || response.Error != "" {
		t.Fatalf("happy response = %+v", response)
	}
	wantCommands := []string{"light brightness 60", "light status"}
	if !reflect.DeepEqual(commands, wantCommands) {
		t.Fatalf("commands = %v, want %v", commands, wantCommands)
	}

	commands = nil
	call = func(command string) (string, error) {
		commands = append(commands, command)
		if command == "light on" {
			return "COM4 left: on 60% 3583K\nCOM7 right: error: timeout", nil
		}
		return "COM4 left: on 60% 3583K\nCOM7 right: on 55% 3583K", nil
	}
	server = newUIServer(42815, newTestDaemonDispatcher(call))
	req = httptest.NewRequest(http.MethodPost, "/api/group", bytes.NewBufferString(`{"action":"on"}`))
	req.Host = "127.0.0.1:42815"
	req.Header.Set("Content-Type", "application/json")
	recorder = httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("partial error status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Details) != 1 || response.Details[0].Error != "error: timeout" {
		t.Fatalf("partial error response = %+v", response)
	}
}

func TestDecodeUIJSONRejectsWrongContentTypeAndTrailingData(t *testing.T) {
	server := newUIServer(42815, newTestDaemonDispatcher(func(string) (string, error) {
		return "no lights known", nil
	}))
	for name, testCase := range map[string]struct {
		contentType string
		body        string
	}{
		"wrong content type": {contentType: "text/plain", body: `{"action":"on"}`},
		"trailing JSON":      {contentType: "application/json", body: `{"action":"on"}{}`},
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/group", strings.NewReader(testCase.body))
			req.Host = "127.0.0.1:42815"
			req.Header.Set("Content-Type", testCase.contentType)
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestProbeUI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/health" {
			t.Fatalf("probe path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	if !probeUI(server.URL + "/") {
		t.Fatal("probeUI() = false for healthy UI")
	}
	if probeUI("http://127.0.0.1:1/") {
		t.Fatal("probeUI() = true for unavailable UI")
	}
}

func TestUITemperatureStepsAreExactAndOrdered(t *testing.T) {
	want := []int{2900, 3128, 3356, 3583, 3811, 4039, 4267, 4494, 4722, 4950, 5178, 5406, 5633, 5861, 6089, 6317, 6544, 6772, 7000}
	if !reflect.DeepEqual(uiTemperatureSteps, want) {
		t.Fatalf("temperature steps = %v, want %v", uiTemperatureSteps, want)
	}
	if !sort.IntsAreSorted(uiTemperatureSteps) {
		t.Fatal("temperature steps are not warm-to-cool sorted")
	}
}

func TestUICommandErrorParsing(t *testing.T) {
	got := parseUICommandErrors("COM4 left: on 50% 2900K\nCOM12 center: error: timeout", "all lights")
	want := []uiDetail{{Target: "COM12 center", Error: "error: timeout"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("details = %+v, want %+v", got, want)
	}
	if got := parseUICommandErrors("error: no light", "all lights"); len(got) != 1 || got[0].Target != "all lights" {
		t.Fatalf("whole-command detail = %+v", got)
	}
}

func TestUIAPIRejectsUnknownActionAndTarget(t *testing.T) {
	server := newUIServer(42815, newTestDaemonDispatcher(func(string) (string, error) {
		return "no lights known", nil
	}))
	for _, body := range []string{
		`{"target":"desk left","action":"on"}`,
		`{"target":"desk-left","action":"explode"}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/light", strings.NewReader(body))
		req.Host = "127.0.0.1:42815"
		req.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("body %s status = %d, want 400", body, recorder.Code)
		}
	}
}

func TestUIAPIRoundTripErrorIs502(t *testing.T) {
	server := newUIServer(42815, newTestDaemonDispatcher(func(string) (string, error) {
		return "", errors.New("connection refused")
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/lights", nil)
	req.Host = "127.0.0.1:42815"
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "connection refused") {
		t.Fatalf("body = %s, want daemon error", recorder.Body.String())
	}
}

func TestRunUIReusesHealthyExistingPanel(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	panel := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/health" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})}
	serveDone := make(chan error, 1)
	go func() { serveDone <- panel.Serve(listener) }()
	defer func() {
		_ = panel.Close()
		<-serveDone
	}()

	port := listener.Addr().(*net.TCPAddr).Port
	url := "http://127.0.0.1:" + strconv.Itoa(port) + "/"
	deadline := time.Now().Add(time.Second)
	for !probeUI(url) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !probeUI(url) {
		t.Fatal("healthy existing panel did not become reachable")
	}

	var out, errOut bytes.Buffer
	if got := runUI([]string{"--port", strconv.Itoa(port), "--no-open"}, &out, &errOut); got != 0 {
		t.Fatalf("runUI existing panel exit = %d, stderr %q", got, errOut.String())
	}
	if strings.Contains(errOut.String(), "cannot listen") {
		t.Fatalf("runUI reported a listen error for healthy existing panel: %q", errOut.String())
	}
	if out.Len() != 0 {
		t.Fatalf("runUI wrote unexpected stdout for reused panel: %q", out.String())
	}
}

func TestRunUIRejectsBadArguments(t *testing.T) {
	var out, errOut bytes.Buffer
	if got := runUI([]string{"--port", "0", "--no-open"}, &out, &errOut); got != 2 {
		t.Fatalf("runUI bad port exit = %d, want 2", got)
	}
	if !strings.Contains(errOut.String(), "between 1 and 65535") {
		t.Fatalf("bad port output = %q", errOut.String())
	}
}

func TestUIResponseJSONIncludesNullUnknownValues(t *testing.T) {
	lights, _ := parseUILightStatus("COM4 desk: off")
	data, err := json.Marshal(uiResponse{Lights: lights})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"brightness":null`)) || !bytes.Contains(data, []byte(`"temp":null`)) {
		t.Fatalf("response JSON = %s, want null off values", data)
	}
}

func TestUIHTTPMethodAndRoot(t *testing.T) {
	server := newUIServer(42815, newTestDaemonDispatcher(func(string) (string, error) {
		return "no lights known", nil
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:42815"
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK || !bytes.Contains(recorder.Body.Bytes(), []byte("<title>Mutastic</title>")) || !bytes.Contains(recorder.Body.Bytes(), []byte("<h1>Mutastic</h1>")) || !bytes.Contains(recorder.Body.Bytes(), []byte(`src="/mutation_queue.js"`)) {
		t.Fatalf("root status/body = %d/%d", recorder.Code, recorder.Body.Len())
	}
	req = httptest.NewRequest(http.MethodGet, "/mutation_queue.js", nil)
	req.Host = "localhost:42815"
	recorder = httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK || !bytes.Contains(recorder.Body.Bytes(), []byte("LightMutationQueue")) {
		t.Fatalf("mutation queue script response = %d/%d", recorder.Code, recorder.Body.Len())
	}
	req = httptest.NewRequest(http.MethodPost, "/api/health", nil)
	req.Host = "localhost:42815"
	recorder = httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("method response = %d, Allow=%q", recorder.Code, recorder.Header().Get("Allow"))
	}
}

func TestBuildGroupPlanRejectsInvalidDelta(t *testing.T) {
	for _, value := range []int{-21, 21} {
		if _, err := buildGroupPlan(uiActionRequest{Action: "brightness-delta", Value: intPointer(value)}); err == nil {
			t.Errorf("brightness delta %d accepted", value)
		}
	}
	for _, value := range []int{-4, 4} {
		if _, err := buildGroupPlan(uiActionRequest{Action: "temp-step-delta", Value: intPointer(value)}); err == nil {
			t.Errorf("temperature delta %d accepted", value)
		}
	}
}

func TestUIStatusErrorsDoNotDiscardGoodLights(t *testing.T) {
	lights, details := parseUILightStatus("COM4 good: on 52% 3583K\nnot a light\nCOM7 bad: error: timeout")
	if len(lights) != 2 || len(details) != 2 {
		t.Fatalf("lights/details = %d/%d, want 2/2", len(lights), len(details))
	}
	byPort := make(map[string]uiLight, len(lights))
	for _, light := range lights {
		byPort[light.Port] = light
	}
	if byPort["COM4"].State != "on" {
		t.Fatalf("good light was not retained: %+v", lights)
	}
}

func TestUIProbeDoesNotAcceptWrongHealth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":false}`)
	}))
	defer server.Close()
	if probeUI(server.URL + "/") {
		t.Fatal("probeUI accepted ok=false")
	}
}

func TestUIShutdownEndpointAnswersThenFiresHook(t *testing.T) {
	server := newUIServer(42815, nil)
	stopped := make(chan struct{}, 1)
	server.shutdown = func() { stopped <- struct{}{} }
	req := httptest.NewRequest(http.MethodPost, "/api/shutdown", nil)
	req.Host = "127.0.0.1:42815"
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/shutdown status = %d, want 200", rec.Code)
	}
	var body struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || !body.OK {
		t.Fatalf("POST /api/shutdown body = %q, want {\"ok\":true}", rec.Body.String())
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("shutdown hook was not called after the reply")
	}
}

func TestUIShutdownEndpointGuards(t *testing.T) {
	server := newUIServer(42815, nil)
	server.shutdown = func() {}
	// Method guard: GET is not a stop verb.
	get := httptest.NewRequest(http.MethodGet, "/api/shutdown", nil)
	get.Host = "127.0.0.1:42815"
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, get)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("GET /api/shutdown = %d (Allow %q), want 405 Allow: POST", rec.Code, rec.Header().Get("Allow"))
	}
	// Origin guard: same posture as the panel's other mutating endpoints.
	req := httptest.NewRequest(http.MethodPost, "/api/shutdown", nil)
	req.Host = "127.0.0.1:42815"
	req.Header.Set("Origin", "http://evil.example")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST /api/shutdown with foreign Origin = %d, want 403", rec.Code)
	}
	// Unwired shutdown answers 503, not a crash.
	unwired := newUIServer(42815, nil)
	unwiredReq := httptest.NewRequest(http.MethodPost, "/api/shutdown", nil)
	unwiredReq.Host = "127.0.0.1:42815"
	rec = httptest.NewRecorder()
	unwired.ServeHTTP(rec, unwiredReq)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /api/shutdown unwired = %d, want 503", rec.Code)
	}
}

// TestUIShutdownEndpointIsIdempotent: overlapping shutdown requests are all
// answered, but the drain goroutine spawns exactly once (a second close of
// the shared drain channel would panic).
func TestUIShutdownEndpointIsIdempotent(t *testing.T) {
	server := newUIServer(42815, nil)
	var calls atomic.Int32
	server.shutdown = func() { calls.Add(1) }
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/shutdown", nil)
		req.Host = "127.0.0.1:42815"
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200", i+1, rec.Code)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("shutdown hook fired %d times, want exactly 1", got)
	}
}

// TestUIShutdownStopsTheRealServer is the end-to-end proof of the
// reply-then-exit pattern: a real runUI server on an ephemeral port must
// answer the POST AND exit cleanly afterwards.
func TestUIShutdownStopsTheRealServer(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)

	var out, errOut bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- runUI([]string{"--port", strconv.Itoa(port), "--no-open"}, &out, &errOut) }()

	deadline := time.Now().Add(2 * time.Second)
	for !probeUI(url) {
		if time.Now().After(deadline) {
			t.Fatalf("ui server did not start: stderr %q", errOut.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	resp, err := http.Post(url+"api/shutdown", "", nil)
	if err != nil {
		t.Fatalf("shutdown POST failed before the server could answer: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("shutdown status = %d, want 200 (the reply must land before the server stops)", resp.StatusCode)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("runUI exit = %d, want 0 (stderr %q)", code, errOut.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runUI did not return after /api/shutdown")
	}
	if probeUI(url) {
		t.Fatal("ui server still answers health after shutdown")
	}
}

// TestUIMicStatusReportsDaemonState pins GET /api/mic: the daemon's mic state
// word maps straight through, while a daemon "error:" reply AND a transport
// error both collapse to "unreachable" - always at HTTP 200, always after
// exactly one "status" daemon call.
func TestUIMicStatusReportsDaemonState(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		reply     string
		err       error
		wantState string
	}{
		{name: "muted", reply: "muted", wantState: "muted"},
		{name: "unmuted", reply: "unmuted", wantState: "unmuted"},
		{name: "unknown", reply: "unknown", wantState: "unknown"},
		{name: "daemon error reply", reply: "error: mic device gone", wantState: "unreachable"},
		{name: "transport error", err: errors.New("connection refused"), wantState: "unreachable"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var commands []string
			server := newUIServer(42815, newTestDaemonDispatcher(func(command string) (string, error) {
				commands = append(commands, command)
				return testCase.reply, testCase.err
			}))
			req := httptest.NewRequest(http.MethodGet, "/api/mic", nil)
			req.Host = "127.0.0.1:42815"
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s; GET /api/mic always answers 200", recorder.Code, recorder.Body.String())
			}
			var body struct {
				State string `json:"state"`
				Error string `json:"error"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.State != testCase.wantState || body.Error != "" {
				t.Fatalf("body = %+v, want state %q and no in-band error", body, testCase.wantState)
			}
			if !reflect.DeepEqual(commands, []string{"status"}) {
				t.Fatalf("daemon commands = %v, want exactly [status]", commands)
			}
		})
	}
}

// TestUIMicPostRunsVerbThenFreshStatus pins the POST success order: the verb
// first, then ONE fresh status query, and the reply carries that fresh state
// (F6 - the verb ack alone is not trusted for the card).
func TestUIMicPostRunsVerbThenFreshStatus(t *testing.T) {
	var commands []string
	server := newUIServer(42815, newTestDaemonDispatcher(func(command string) (string, error) {
		commands = append(commands, command)
		switch command {
		case "mute":
			return "muted", nil
		case "status":
			return "muted", nil
		default:
			return "error: unexpected command", nil
		}
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/mic", bytes.NewBufferString(`{"action":"mute"}`))
	req.Host = "127.0.0.1:42815"
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		State string `json:"state"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.State != "muted" || body.Error != "" {
		t.Fatalf("body = %+v, want {\"state\":\"muted\"}", body)
	}
	if !reflect.DeepEqual(commands, []string{"mute", "status"}) {
		t.Fatalf("daemon commands = %v, want [mute status] (verb, then ONE fresh status query)", commands)
	}
}

// TestUIMicPostMapsDaemonFailuresTo502 pins the F6 failure mapping: a verb
// reply of exactly one "error:" line, or a transport error on the verb,
// answers HTTP 502 and SKIPS the fresh status query (a 200 there would make
// the mutation queue treat a mute that never happened as success).
func TestUIMicPostMapsDaemonFailuresTo502(t *testing.T) {
	t.Run("one line daemon error reply", func(t *testing.T) {
		var commands []string
		server := newUIServer(42815, newTestDaemonDispatcher(func(command string) (string, error) {
			commands = append(commands, command)
			return "error: mic device gone", nil
		}))
		req := httptest.NewRequest(http.MethodPost, "/api/mic", bytes.NewBufferString(`{"action":"mute"}`))
		req.Host = "127.0.0.1:42815"
		req.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, body %s; want 502", recorder.Code, recorder.Body.String())
		}
		var body struct {
			State string `json:"state"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Error != "error: mic device gone" || body.State != "" {
			t.Fatalf("body = %+v, want the daemon's error line verbatim and no state", body)
		}
		if !reflect.DeepEqual(commands, []string{"mute"}) {
			t.Fatalf("daemon commands = %v, want [mute] - the fresh status query must be skipped on verb failure", commands)
		}
	})
	t.Run("transport failure on the verb", func(t *testing.T) {
		var commands []string
		server := newUIServer(42815, newTestDaemonDispatcher(func(command string) (string, error) {
			commands = append(commands, command)
			return "", errors.New("connection refused")
		}))
		req := httptest.NewRequest(http.MethodPost, "/api/mic", bytes.NewBufferString(`{"action":"toggle"}`))
		req.Host = "127.0.0.1:42815"
		req.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, body %s; want 502", recorder.Code, recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), "connection refused") {
			t.Fatalf("body = %s, want the transport error", recorder.Body.String())
		}
		if !reflect.DeepEqual(commands, []string{"toggle"}) {
			t.Fatalf("daemon commands = %v, want [toggle] - no status query after a transport failure", commands)
		}
	})
	t.Run("multi line reply is not a verb failure", func(t *testing.T) {
		var commands []string
		server := newUIServer(42815, newTestDaemonDispatcher(func(command string) (string, error) {
			commands = append(commands, command)
			if command == "mute" {
				return "muted\nerror: trailing noise", nil
			}
			return "muted", nil
		}))
		req := httptest.NewRequest(http.MethodPost, "/api/mic", bytes.NewBufferString(`{"action":"mute"}`))
		req.Host = "127.0.0.1:42815"
		req.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s; only an exactly-one-line error: reply is a verb failure", recorder.Code, recorder.Body.String())
		}
		if !reflect.DeepEqual(commands, []string{"mute", "status"}) {
			t.Fatalf("daemon commands = %v, want [mute status]", commands)
		}
	})
}

// TestUIMicPostValidatesActionHonorsGuardsAndMethods pins the route's guard
// posture: bad or missing actions answer 400 and NEVER reach the daemon, a
// foreign Origin answers 403, the origin check fires BEFORE action
// validation (a bad action with a foreign Origin still answers 403), and
// wrong methods answer 405 with Allow: GET, POST.
func TestUIMicPostValidatesActionHonorsGuardsAndMethods(t *testing.T) {
	var calls int
	server := newUIServer(42815, newTestDaemonDispatcher(func(string) (string, error) {
		calls++
		return "unknown", nil
	}))
	post := func(body, origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/mic", strings.NewReader(body))
		req.Host = "127.0.0.1:42815"
		req.Header.Set("Content-Type", "application/json")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, req)
		return recorder
	}
	for _, body := range []string{`{"action":"explode"}`, `{"action":""}`, `{}`} {
		if got := post(body, "").Code; got != http.StatusBadRequest {
			t.Fatalf("body %s status = %d, want 400", body, got)
		}
	}
	if calls != 0 {
		t.Fatalf("invalid actions reached the daemon %d times; want zero calls", calls)
	}
	if got := post(`{"action":"mute"}`, "http://evil.example").Code; got != http.StatusForbidden {
		t.Fatalf("foreign Origin status = %d, want 403", got)
	}
	if calls != 0 {
		t.Fatalf("a foreign-Origin POST reached the daemon; want zero calls")
	}
	if got := post(`{"action":"explode"}`, "http://evil.example").Code; got != http.StatusForbidden {
		t.Fatalf("bad action with a foreign Origin status = %d, want 403 - the origin check must fire before action validation", got)
	}
	if calls != 0 {
		t.Fatalf("a foreign-Origin bad-action POST reached the daemon; want zero calls")
	}
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/api/mic", nil)
		req.Host = "127.0.0.1:42815"
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != "GET, POST" {
			t.Fatalf("%s status = %d Allow %q, want 405 Allow: GET, POST", method, recorder.Code, recorder.Header().Get("Allow"))
		}
	}
	if calls != 0 {
		t.Fatalf("wrong-method requests reached the daemon; want zero calls")
	}
}

// TestUIAPISettingsList pins GET /api/settings: exactly one
// "light settings list" call per request, a REAL names array in every reply
// (an empty store's "" reply parses to [], never null), and F5 in-band
// degradation at HTTP 200 - a daemon TRANSPORT failure answers
// {"names":[],"error":"unreachable"} while a daemon single-line "error:"
// refusal (e.g. a disabled or corrupt store) is carried through verbatim.
func TestUIAPISettingsList(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		reply     string
		err       error
		wantNames []string
		wantError string
	}{
		{name: "daemon names in order", reply: "focus\nmovie mode", wantNames: []string{"focus", "movie mode"}},
		{name: "empty store replies a real empty array", reply: "", wantNames: []string{}},
		{name: "transport failure degrades in band", err: errors.New("connection refused"), wantNames: []string{}, wantError: "unreachable"},
		{name: "daemon error reply degrades in band verbatim", reply: "error: settings persistence disabled", wantNames: []string{}, wantError: "error: settings persistence disabled"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var commands []string
			server := newUIServer(42815, newTestDaemonDispatcher(func(command string) (string, error) {
				commands = append(commands, command)
				return testCase.reply, testCase.err
			}))
			req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
			req.Host = "127.0.0.1:42815"
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s; GET /api/settings always answers 200", recorder.Code, recorder.Body.String())
			}
			var body struct {
				Names  []string `json:"names"`
				Error  string   `json:"error"`
				Detail string   `json:"detail"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Names == nil {
				t.Fatalf("names must be a REAL array, never null/omitted: body %s", recorder.Body.String())
			}
			if !reflect.DeepEqual(body.Names, testCase.wantNames) {
				t.Fatalf("names = %v, want %v", body.Names, testCase.wantNames)
			}
			if body.Error != testCase.wantError || body.Detail != "" {
				t.Fatalf("error/detail = %q / %q, want error %q and no detail", body.Error, body.Detail, testCase.wantError)
			}
			if len(testCase.wantNames) == 0 && !strings.Contains(recorder.Body.String(), `"names":[]`) {
				t.Fatalf("body %s must contain a literal \"names\":[]", recorder.Body.String())
			}
			if !reflect.DeepEqual(commands, []string{"light settings list"}) {
				t.Fatalf("daemon commands = %v, want exactly [light settings list]", commands)
			}
		})
	}
}

// TestUIAPISettingsSaveAndApplyRefreshTheList pins the POST order contract:
// exactly [light settings <verb> <name>, light settings list] and HTTP 200
// with the refreshed names in the POST reply itself (the page never re-GETs
// after a POST). A successful apply additionally carries the full verbatim
// daemon reply as detail. The F6 carve-out - mutation COMMITTED but the
// follow-up refresh failed (transport error OR single-line "error:" reply) -
// answers 200 with names null/omitted plus an error naming the REFRESH
// failure; it is never a 502. The retry row pins that a delete retried after
// a committed-but-unacknowledged first delete surfaces the daemon's
// "error: unknown setting" verbatim as a 502.
func TestUIAPISettingsSaveAndApplyRefreshTheList(t *testing.T) {
	post := func(server *uiServer, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(body))
		req.Host = "127.0.0.1:42815"
		req.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, req)
		return recorder
	}
	for _, testCase := range []struct {
		verb       string
		verbReply  string
		wantDetail bool
	}{
		{verb: "save", verbReply: `saved "movie mode" (2 lights)`},
		{verb: "apply", verbReply: "COM4 desk: on 47% 2900K\nCOM7: off", wantDetail: true},
		{verb: "delete", verbReply: `deleted "movie mode"`},
	} {
		t.Run(testCase.verb+" refreshes the list after the verb", func(t *testing.T) {
			var commands []string
			server := newUIServer(42815, newTestDaemonDispatcher(func(command string) (string, error) {
				commands = append(commands, command)
				switch command {
				case "light settings " + testCase.verb + " movie mode":
					return testCase.verbReply, nil
				case "light settings list":
					return "focus\nmovie mode", nil
				default:
					return "", fmt.Errorf("unexpected command %q", command)
				}
			}))
			recorder := post(server, `{"action":"`+testCase.verb+`","name":"movie mode"}`)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body.String())
			}
			var body struct {
				Names  []string `json:"names"`
				Error  string   `json:"error"`
				Detail string   `json:"detail"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(body.Names, []string{"focus", "movie mode"}) {
				t.Fatalf("names = %v, want the refreshed [focus movie mode]", body.Names)
			}
			if body.Error != "" {
				t.Fatalf("error = %q, want none on a clean mutation+refresh", body.Error)
			}
			wantDetail := ""
			if testCase.wantDetail {
				wantDetail = testCase.verbReply
			}
			if body.Detail != wantDetail {
				t.Fatalf("detail = %q, want %q", body.Detail, wantDetail)
			}
			wantCommands := []string{"light settings " + testCase.verb + " movie mode", "light settings list"}
			if !reflect.DeepEqual(commands, wantCommands) {
				t.Fatalf("daemon commands = %v, want %v (verb first, then exactly ONE list refresh)", commands, wantCommands)
			}
		})
		t.Run(testCase.verb+" mutation committed with failed refresh answers 200", func(t *testing.T) {
			for _, refreshFailure := range []struct {
				name   string
				reply  string
				err    error
				substr string
			}{
				{name: "refresh transport error", err: errors.New("connection refused"), substr: "connection refused"},
				{name: "refresh daemon error reply", reply: "error: settings persistence disabled", substr: "error: settings persistence disabled"},
			} {
				t.Run(refreshFailure.name, func(t *testing.T) {
					var commands []string
					server := newUIServer(42815, newTestDaemonDispatcher(func(command string) (string, error) {
						commands = append(commands, command)
						if command == "light settings list" {
							return refreshFailure.reply, refreshFailure.err
						}
						return testCase.verbReply, nil
					}))
					recorder := post(server, `{"action":"`+testCase.verb+`","name":"movie mode"}`)
					if recorder.Code != http.StatusOK {
						t.Fatalf("status = %d, body %s; a failed REFRESH after a committed mutation is never a 502", recorder.Code, recorder.Body.String())
					}
					var body struct {
						Names  []string `json:"names"`
						Error  string   `json:"error"`
						Detail string   `json:"detail"`
					}
					if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
						t.Fatal(err)
					}
					if body.Names != nil {
						t.Fatalf("names = %v, want null/omitted when the follow-up refresh failed", body.Names)
					}
					if !strings.Contains(body.Error, "settings list refresh failed") || !strings.Contains(body.Error, refreshFailure.substr) {
						t.Fatalf("error = %q, want a refresh failure naming %q", body.Error, refreshFailure.substr)
					}
					wantDetail := ""
					if testCase.wantDetail {
						wantDetail = testCase.verbReply
					}
					if body.Detail != wantDetail {
						t.Fatalf("detail = %q, want %q", body.Detail, wantDetail)
					}
					wantCommands := []string{"light settings " + testCase.verb + " movie mode", "light settings list"}
					if !reflect.DeepEqual(commands, wantCommands) {
						t.Fatalf("daemon commands = %v, want %v", commands, wantCommands)
					}
				})
			}
		})
	}
	t.Run("delete retried after a committed but unacknowledged delete", func(t *testing.T) {
		var commands []string
		server := newUIServer(42815, newTestDaemonDispatcher(func(command string) (string, error) {
			commands = append(commands, command)
			switch {
			case command == "light settings delete movie mode" && len(commands) == 1:
				// First attempt COMMITS on the daemon ...
				return `deleted "movie mode"`, nil
			case command == "light settings delete movie mode":
				// ... but the retry arrives after the commit: the daemon refuses verbatim.
				return `error: unknown setting "movie mode"`, nil
			case command == "light settings list":
				// The first attempt's refresh fails (that is the "unacknowledged" half).
				return "", errors.New("connection refused")
			default:
				return "", fmt.Errorf("unexpected command %q", command)
			}
		}))
		first := post(server, `{"action":"delete","name":"movie mode"}`)
		if first.Code != http.StatusOK {
			t.Fatalf("first delete status = %d, body %s; committed mutation with failed refresh must be 200", first.Code, first.Body.String())
		}
		second := post(server, `{"action":"delete","name":"movie mode"}`)
		if second.Code != http.StatusBadGateway {
			t.Fatalf("retried delete status = %d, body %s; the daemon's unknown-setting refusal must surface as 502", second.Code, second.Body.String())
		}
		var body struct {
			Names []string `json:"names"`
			Error string   `json:"error"`
		}
		if err := json.Unmarshal(second.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Error != `error: unknown setting "movie mode"` {
			t.Fatalf("error = %q, want the daemon's refusal verbatim", body.Error)
		}
		wantCommands := []string{"light settings delete movie mode", "light settings list", "light settings delete movie mode"}
		if !reflect.DeepEqual(commands, wantCommands) {
			t.Fatalf("daemon commands = %v, want %v (no refresh after a failed verb)", commands, wantCommands)
		}
	})
}

// TestUIAPISettingsValidatesAndPassesDaemonErrorsThrough pins the guard and
// failure posture: client-side validation (unknown action; empty, whitespace-
// only, or newline-bearing names) answers 400 and NEVER reaches the daemon;
// the origin check fires before action validation; wrong methods answer 405
// Allow: GET, POST; a single-line "error:" mutation reply on save/delete
// answers 502 verbatim with NO follow-up refresh; the apply reply is
// classified LINE-WISE (LB-4) - all-error-lines (a two-port setting with both
// ports unreachable) is 502 with the verbatim reply, while the one-vs-two
// unreachable discrimination (one success line + one inline skip error) is
// 200 with refreshed names AND the full reply carried as detail.
func TestUIAPISettingsValidatesAndPassesDaemonErrorsThrough(t *testing.T) {
	var calls int
	server := newUIServer(42815, newTestDaemonDispatcher(func(string) (string, error) {
		calls++
		return "", nil
	}))
	post := func(body, origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(body))
		req.Host = "127.0.0.1:42815"
		req.Header.Set("Content-Type", "application/json")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, req)
		return recorder
	}
	for _, body := range []string{
		`{"action":"explode","name":"x"}`,
		`{"action":"","name":"x"}`,
		`{"name":"x"}`,
		`{}`,
		`{"action":"save","name":""}`,
		`{"action":"apply","name":"   "}`,
		`{"action":"delete","name":" \t "}`,
		`{"action":"delete","name":"movie\nmode"}`,
	} {
		if got := post(body, "").Code; got != http.StatusBadRequest {
			t.Fatalf("body %s status = %d, want 400", body, got)
		}
	}
	if calls != 0 {
		t.Fatalf("invalid actions/names reached the daemon %d times; want zero calls", calls)
	}
	if got := post(`{"action":"save","name":"x"}`, "http://evil.example").Code; got != http.StatusForbidden {
		t.Fatalf("foreign Origin status = %d, want 403", got)
	}
	if calls != 0 {
		t.Fatalf("a foreign-Origin POST reached the daemon; want zero calls")
	}
	if got := post(`{"action":"explode"}`, "http://evil.example").Code; got != http.StatusForbidden {
		t.Fatalf("bad action with a foreign Origin status = %d, want 403 - the origin check must fire before validation", got)
	}
	if calls != 0 {
		t.Fatalf("a foreign-Origin bad-action POST reached the daemon; want zero calls")
	}
	for _, method := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(method, "/api/settings", nil)
		req.Host = "127.0.0.1:42815"
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != "GET, POST" {
			t.Fatalf("%s status = %d Allow %q, want 405 Allow: GET, POST", method, recorder.Code, recorder.Header().Get("Allow"))
		}
	}
	if calls != 0 {
		t.Fatalf("wrong-method requests reached the daemon; want zero calls")
	}
}

// TestUIAPISettingsNameByteCap pins R4-F3: POST /api/settings enforces the
// daemon store's 42-BYTE name cap server-side for EVERY action, before any
// daemon call - the page's JS gate (R3-F6) is bypassable by a direct caller.
// (Wire backdrop: since R7-F3 the daemon's 128-byte receive buffer lets a
// 65-127-byte over-cap name arrive whole and draw the daemon's documented
// rejection everywhere; since R8-F1 a datagram FILLING or exceeding the
// buffer is refused "error: command too long" without dispatch on Unix,
// while on Windows the oversized read still fails and the client times
// out - WSAEMSGSIZE; pre-R7-F3 a 65-byte delete TRUNCATED into
// its 42-byte prefix on Unix, a destructive mis-delete.) A 42-byte
// name passes through to the daemon; 43 bytes - plain ASCII, or a
// 15-character CJK name at 45 UTF-8 bytes - answers HTTP 400 with ZERO
// daemon calls. The daemon's own identical check stays authoritative.
func TestUIAPISettingsNameByteCap(t *testing.T) {
	post := func(server *uiServer, action, name string) *httptest.ResponseRecorder {
		// The pinned names contain no JSON-significant characters, so the
		// body can be built inline.
		body := `{"action":"` + action + `","name":"` + name + `"}`
		req := httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(body))
		req.Host = "127.0.0.1:42815"
		req.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, req)
		return recorder
	}
	for _, action := range []string{"save", "apply", "delete"} {
		t.Run(action+" with a 42-byte name reaches the daemon", func(t *testing.T) {
			name := strings.Repeat("a", 42)
			if len(name) != uiMaxSettingsNameBytes {
				t.Fatalf("pin sanity: %d bytes, want the inclusive cap %d", len(name), uiMaxSettingsNameBytes)
			}
			var commands []string
			server := newUIServer(42815, newTestDaemonDispatcher(func(command string) (string, error) {
				commands = append(commands, command)
				if strings.HasPrefix(command, "light settings apply ") {
					return "COM4: on 47% 2900K", nil
				}
				if command == "light settings list" {
					return name, nil
				}
				return `saved "x" (1 lights)`, nil
			}))
			recorder := post(server, action, name)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s; a name AT the byte cap must pass to the daemon", recorder.Code, recorder.Body.String())
			}
			wantCommands := []string{"light settings " + action + " " + name, "light settings list"}
			if !reflect.DeepEqual(commands, wantCommands) {
				t.Fatalf("daemon commands = %v, want %v", commands, wantCommands)
			}
		})
		for _, overCapName := range []struct {
			label string
			name  string
		}{
			{"43-byte ASCII", strings.Repeat("b", 43)},
			{"15-char CJK (45 UTF-8 bytes)", strings.Repeat("中", 15)},
		} {
			t.Run(action+" with a "+overCapName.label+" name is 400 before any daemon call", func(t *testing.T) {
				if len(overCapName.name) <= uiMaxSettingsNameBytes {
					t.Fatalf("pin sanity: %d bytes must exceed the %d-byte cap", len(overCapName.name), uiMaxSettingsNameBytes)
				}
				calls := 0
				server := newUIServer(42815, newTestDaemonDispatcher(func(string) (string, error) {
					calls++
					return "", nil
				}))
				recorder := post(server, action, overCapName.name)
				if recorder.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, body %s; want 400", recorder.Code, recorder.Body.String())
				}
				if !strings.Contains(recorder.Body.String(), "settings name too long (max 42 bytes)") {
					t.Fatalf("body %s, want the byte-cap validation error", recorder.Body.String())
				}
				if calls != 0 {
					t.Fatalf("an over-cap %s reached the daemon %d times; want zero calls", action, calls)
				}
			})
		}
	}
}

// TestUIAPISettingsRawUTF8Gate pins R11-F3: POST /api/settings validates
// the RAW bounded body with utf8.Valid BEFORE the JSON decoder - for EVERY
// action - because encoding/json silently coerces invalid UTF-8 inside JSON
// strings to U+FFFD, and the daemon-side utf8.ValidString name check would
// then only ever see replacement characters (a direct POST saving,
// applying, or deleting a raw "a\x80b" would otherwise act on the DISTINCT
// existing name "a�b"). A raw invalid body answers HTTP 400 "invalid
// request encoding" with ZERO daemon calls; a legal multi-byte UTF-8 name
// passes through to the daemon byte-for-byte unchanged.
func TestUIAPISettingsRawUTF8Gate(t *testing.T) {
	post := func(server *uiServer, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/settings", bytes.NewReader(body))
		req.Host = "127.0.0.1:42815"
		req.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, req)
		return recorder
	}
	for _, action := range []string{"save", "apply", "delete"} {
		t.Run(action+" with raw invalid UTF-8 in the body is 400 before any daemon call", func(t *testing.T) {
			// A lone 0x80 continuation byte inside the JSON name string:
			// valid JSON framing, invalid UTF-8 - the decoder would
			// coerce it to U+FFFD and smuggle a distinct name through.
			// (Double-quoted literal: the \x80 escape must be REAL bytes.)
			body := []byte("{\"action\":\"" + action + "\",\"name\":\"a\x80b\"}")
			if utf8.Valid(body) {
				t.Fatal("pin sanity: the crafted body must be INVALID UTF-8")
			}
			calls := 0
			server := newUIServer(42815, newTestDaemonDispatcher(func(string) (string, error) {
				calls++
				return "", nil
			}))
			recorder := post(server, body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body %s; want 400", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), "invalid request encoding") {
				t.Fatalf("body %s, want the raw-encoding refusal", recorder.Body.String())
			}
			if calls != 0 {
				t.Fatalf("an invalid-encoding %s reached the daemon %d times; want zero calls", action, calls)
			}
		})
		t.Run(action+" with a legal multi-byte name passes through unchanged", func(t *testing.T) {
			name := "café 中 🚀"
			body := []byte(`{"action":"` + action + `","name":"` + name + `"}`)
			if !utf8.Valid(body) {
				t.Fatal("pin sanity: the multi-byte body must be VALID UTF-8")
			}
			var commands []string
			server := newUIServer(42815, newTestDaemonDispatcher(func(command string) (string, error) {
				commands = append(commands, command)
				if strings.HasPrefix(command, "light settings apply ") {
					return "COM4: on 47% 2900K", nil
				}
				if command == "light settings list" {
					return name, nil
				}
				return `saved "x" (1 lights)`, nil
			}))
			recorder := post(server, body)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s; a legal multi-byte name must pass the encoding gate", recorder.Code, recorder.Body.String())
			}
			wantCommands := []string{"light settings " + action + " " + name, "light settings list"}
			if !reflect.DeepEqual(commands, wantCommands) {
				t.Fatalf("daemon commands = %v, want %v (the name forwarded BYTE-EXACT)", commands, wantCommands)
			}
		})
	}
}

// TestCountUIApplySuccesses pins the line-wise apply classifier directly:
// BOTH daemon failure line shapes count as failures (a line-initial "error:"
// skip/refusal AND the label-prefixed per-light failure like
// "COM7: error: timeout" from callLight/Manager.apply in the settingsApply
// fan-out), and the classifier can never misfire on the daemon's well-known
// good lines ("COM4 desk: on 47% 2900K" - the label is a COM port plus an
// [a-z0-9-] registry name and StatusString renders no colon; a
// `saved "x" (2 lights)` mutation reply contains no ": error:" either).
func TestCountUIApplySuccesses(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		reply string
		want  int
	}{
		{name: "good status lines only", reply: "COM4 desk: on 47% 2900K\nCOM7: off\nCOM12 left: unknown", want: 3},
		{name: "saved mutation reply never misfires", reply: `saved "work" (2 lights)`, want: 1},
		{name: "line-initial error skip lines are failures", reply: "error: light \"COM4\": unreachable, skipped\nerror: light \"COM9\": unreachable, skipped", want: 0},
		{name: "label-prefixed per-light failures are failures", reply: "COM4 desk: error: timeout\nCOM7: error: simulated write failure", want: 0},
		{name: "mixed reply counts only the good lines", reply: "COM4 desk: on 47% 2900K\nCOM7: error: timeout\nCOM12 left: off", want: 2},
		// The applySaved aggregation's per-key step failure line - the same
		// label-prefixed shape with a step name inside the error text.
		{name: "aggregated sub-step failure lines are failures", reply: "COM4 desk: on 47% 2900K\nCOM7 desk: error: brightness: scripted write failure", want: 1},
		{name: "blank lines never count", reply: "\nCOM4: on 47% 2900K\n\n", want: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := countUIApplySuccesses(testCase.reply); got != testCase.want {
				t.Fatalf("countUIApplySuccesses(%q) = %d, want %d", testCase.reply, got, testCase.want)
			}
		})
	}
}

// TestUIAPISettingsDaemonFailuresAre502 pins the 502 posture: 502 is reserved
// for MUTATION failures only - a transport error on the verb, a single-line
// "error:" reply on save/delete, or an apply reply with ZERO success lines.
// Each 502 carries the failure verbatim and skips the follow-up refresh.
func TestUIAPISettingsDaemonFailuresAre502(t *testing.T) {
	post := func(server *uiServer, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(body))
		req.Host = "127.0.0.1:42815"
		req.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, req)
		return recorder
	}
	for _, testCase := range []struct {
		name        string
		verb        string
		settingName string
		verbReply   string
		verbErr     error
		wantError   string
	}{
		{name: "save one line daemon error", verb: "save", settingName: "x", verbReply: "error: too many saved settings (max 100)", wantError: "error: too many saved settings (max 100)"},
		{name: "save disabled store refusal", verb: "save", settingName: "x", verbReply: "error: settings persistence disabled", wantError: "error: settings persistence disabled"},
		{name: "delete one line daemon error", verb: "delete", settingName: "nope", verbReply: `error: unknown setting "nope"`, wantError: `error: unknown setting "nope"`},
		{name: "apply zero success lines is 502 verbatim", verb: "apply", settingName: "movie mode", verbReply: "error: light \"COM4\": unreachable, skipped\nerror: light \"COM9\": unreachable, skipped", wantError: "error: light \"COM4\": unreachable, skipped\nerror: light \"COM9\": unreachable, skipped"},
		{name: "apply all label-prefixed per-light failures is 502 verbatim", verb: "apply", settingName: "movie mode", verbReply: "COM4 desk: error: timeout\nCOM7: error: simulated write failure", wantError: "COM4 desk: error: timeout\nCOM7: error: simulated write failure"},
		{name: "apply single line error reply", verb: "apply", settingName: "nope", verbReply: `error: unknown setting "nope"`, wantError: `error: unknown setting "nope"`},
		{name: "transport failure on the verb", verb: "apply", settingName: "x", verbErr: errors.New("connection refused"), wantError: "connection refused"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var commands []string
			server := newUIServer(42815, newTestDaemonDispatcher(func(command string) (string, error) {
				commands = append(commands, command)
				return testCase.verbReply, testCase.verbErr
			}))
			recorder := post(server, `{"action":"`+testCase.verb+`","name":"`+testCase.settingName+`"}`)
			if recorder.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, body %s; want 502", recorder.Code, recorder.Body.String())
			}
			var body struct {
				Names []string `json:"names"`
				Error string   `json:"error"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error != testCase.wantError {
				t.Fatalf("error = %q, want the failure verbatim %q", body.Error, testCase.wantError)
			}
			if len(body.Names) != 0 {
				t.Fatalf("names = %v, want empty on a 502", body.Names)
			}
			wantCommands := []string{"light settings " + testCase.verb + " " + testCase.settingName}
			if !reflect.DeepEqual(commands, wantCommands) {
				t.Fatalf("daemon commands = %v, want %v - the follow-up refresh must be skipped on verb failure", commands, wantCommands)
			}
		})
	}
	t.Run("apply with one reachable port is 200 with detail", func(t *testing.T) {
		// The one-vs-two unreachable discrimination (LB-4): ONE success line
		// plus ONE inline skip error is a partial success - HTTP 200 with the
		// refreshed names AND the full verbatim reply as detail, rendered in
		// the page's error banner (never hidden).
		verbReply := "COM4 desk: on 47% 2900K\nerror: light \"COM9\": unreachable, skipped"
		var commands []string
		server := newUIServer(42815, newTestDaemonDispatcher(func(command string) (string, error) {
			commands = append(commands, command)
			if command == "light settings list" {
				return "focus", nil
			}
			return verbReply, nil
		}))
		recorder := post(server, `{"action":"apply","name":"movie mode"}`)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s; a partially successful apply is 200, never 502", recorder.Code, recorder.Body.String())
		}
		var body struct {
			Names  []string `json:"names"`
			Error  string   `json:"error"`
			Detail string   `json:"detail"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(body.Names, []string{"focus"}) || body.Error != "" {
			t.Fatalf("names/error = %v / %q, want [focus] and no error", body.Names, body.Error)
		}
		if body.Detail != verbReply {
			t.Fatalf("detail = %q, want the full verbatim reply %q (partial success is never hidden)", body.Detail, verbReply)
		}
		wantCommands := []string{"light settings apply movie mode", "light settings list"}
		if !reflect.DeepEqual(commands, wantCommands) {
			t.Fatalf("daemon commands = %v, want %v", commands, wantCommands)
		}
	})
	t.Run("apply with one label-prefixed per-light failure between successes is 200 with detail", func(t *testing.T) {
		// The daemon's OTHER real failure shape (callLight's 2 s timeout or
		// a write failure rendered as mm.label(key)+": "+reply in the
		// settingsApply fan-out): COM7: error: timeout counts as a FAILURE
		// for the success tally, never as success. Two good lines keep the
		// apply at 200 with the full verbatim reply carried as detail, so the
		// page's error banner can show the failure line (never hidden).
		verbReply := "COM4 desk: on 47% 2900K\nCOM7: error: timeout\nCOM12 left: off"
		var commands []string
		server := newUIServer(42815, newTestDaemonDispatcher(func(command string) (string, error) {
			commands = append(commands, command)
			if command == "light settings list" {
				return "focus", nil
			}
			return verbReply, nil
		}))
		recorder := post(server, `{"action":"apply","name":"movie mode"}`)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s; a partially successful apply is 200, never 502", recorder.Code, recorder.Body.String())
		}
		var body struct {
			Names  []string `json:"names"`
			Error  string   `json:"error"`
			Detail string   `json:"detail"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(body.Names, []string{"focus"}) || body.Error != "" {
			t.Fatalf("names/error = %v / %q, want [focus] and no error", body.Names, body.Error)
		}
		if body.Detail != verbReply {
			t.Fatalf("detail = %q, want the full verbatim reply %q (partial success is never hidden)", body.Detail, verbReply)
		}
		if !strings.Contains(body.Detail, "COM7: error: timeout") {
			t.Fatalf("detail = %q, want it to carry the daemon's failure line verbatim", body.Detail)
		}
		wantCommands := []string{"light settings apply movie mode", "light settings list"}
		if !reflect.DeepEqual(commands, wantCommands) {
			t.Fatalf("daemon commands = %v, want %v", commands, wantCommands)
		}
	})
}

// TestEmbeddedLightUIHasSavedSettingsSection pins the saved-settings card's
// wiring contract: the section sits between the gang controls and the
// individual controls; the Save form posts through the mutation queue (never
// a direct fetch POST); every rendered name passes through escapeHTML and
// round-trips verbatim through data-apply/data-delete; the POST reply's names
// re-render the list from the queue's onSuccess; a successful apply also
// triggers refreshLights(true) and renders the daemon's detail in the error
// banner so a partial apply is never hidden; and the list refetches on the
// shared 750 ms poll (the F6 refresh-failure recovery path).
func TestEmbeddedLightUIHasSavedSettingsSection(t *testing.T) {
	source := string(lightUIHTML)
	for _, fragment := range []string{
		`id="preset-dd"`,
		`id="settings-form"`,
		`id="settings-name"`,
		`id="settings-list"`,
		`id="settings-empty"`,
		`function renderSettings(names) {`,
		`function refreshSettings() {`,
		`function bindSettingsControls() {`,
		`const escaped = escapeHTML(name);`,
		`data-apply="${escaped}"`,
		`data-delete="${escaped}"`,
		`aria-label="Delete ${escaped}">✕</button>`,
		`renderSettings(data.names || []);`,
		`renderSettings(result.names);`,
		`if (name.trim() === "") return;`,
		`if (action === "apply") {`,
		`function showApplyDetail(detail) {`,
		`window.setInterval(() => { refreshLights(true); refreshMic(); refreshSettings(); }, 750);`,
	} {
		if !strings.Contains(source, fragment) {
			t.Fatalf("embedded UI is missing saved-settings fragment %q", fragment)
		}
	}
	for _, fragment := range []string{
		"enqueueMutation(\"settings:save\", \"/api/settings\", {action: \"save\", name}, false);",
		"enqueueMutation(`settings:apply:${name}`, \"/api/settings\", {action: \"apply\", name}, false);",
		"enqueueMutation(`settings:delete:${name}`, \"/api/settings\", {action: \"delete\", name}, false);",
		"refreshLights(true);\n            showApplyDetail(result.detail);",
		`function flushPendingSliders() {`,
		// R3-F6: the in-page UTF-8 byte-length gate (before any network
		// call) with the daemon's own too-long message.
		`function settingsNameOverByteCap(name) {`,
		`new TextEncoder().encode(name).length > 42`,
		`const SETTINGS_NAME_TOO_LONG = "error: settings name too long (max 42 bytes)";`,
	} {
		if !strings.Contains(source, fragment) {
			t.Fatalf("embedded UI is missing saved-settings queue fragment %q", fragment)
		}
	}
	// Both name-taking mutation entry points (Save form, row Delete) reject
	// an over-byte-cap name in-page with the daemon's message.
	if got := strings.Count(source, "if (settingsNameOverByteCap(name)) {"); got != 2 {
		t.Fatalf("settingsNameOverByteCap gates = %d, want exactly 2 (save, delete)", got)
	}
	if got := strings.Count(source, "showError(SETTINGS_NAME_TOO_LONG);"); got != 2 {
		t.Fatalf("showError(SETTINGS_NAME_TOO_LONG) call sites = %d, want exactly 2 (save, delete)", got)
	}
	// R2-F4: every settings mutation entry point (Save, Apply, Delete)
	// flushes the pending slider debounces BEFORE enqueueing, so the queued
	// settings operation preserves the user's latest slider input.
	if got := strings.Count(source, "flushPendingSliders();"); got != 3 {
		t.Fatalf("flushPendingSliders(); call sites = %d, want exactly 3 (save, apply, delete)", got)
	}
	if directPost := regexp.MustCompile(`fetch\(\s*"/api/settings"\s*,\s*\{\s*method:\s*"POST"`).FindStringSubmatch(source); directPost != nil {
		t.Fatalf("settings mutations must go through the mutation queue, never a direct fetch POST (matched %q)", directPost[0])
	}
	gangControls := strings.Index(source, `id="gang-toggle"`)
	settings := strings.Index(source, `id="preset-dd"`)
	individual := strings.Index(source, `id="lights-grid"`)
	if gangControls < 0 || settings < 0 || individual < 0 || gangControls >= settings || settings >= individual {
		t.Fatalf("the saved-settings control must sit between the gang controls and the individual controls (offsets %d / %d / %d)", gangControls, settings, individual)
	}
}

// TestEmbeddedLightUISettingsSectionBehaviorNodeDOMStub EXECUTES the embedded
// page script under Node against the shared hand-rolled DOM stub (NOT a
// browser, no new dependencies) and pins the settings card's runtime
// behavior: a scripted two-name list renders the daemon's names in order with
// each name round-tripped verbatim into data-apply/data-delete; each row's
// Apply/Delete enqueues exactly the right /api/settings payload through the
// mutation queue and nothing else; and Save with an EMPTY or whitespace-only
// name never issues a mutation (F8).
func TestEmbeddedLightUISettingsSectionBehaviorNodeDOMStub(t *testing.T) {
	runPageScriptWithDOMStub(t, `
stubFetchScript({
	"/api/lights": {status: 200, body: {lights: []}},
	"/api/mic": {status: 200, body: {state: "unmuted"}},
	"/api/settings": {status: 200, body: {names: ["alpha", "beta"]}},
	"POST /api/settings": {status: 200, body: {names: ["alpha", "beta"]}},
});
intervalCallback();
await flush();
const listHTML = document.getElementById("settings-list").innerHTML;
assert.equal(listHTML.includes('data-apply="alpha"'), true, "the alpha row must render with its name round-tripped into data-apply");
assert.equal(listHTML.includes('data-apply="beta"'), true, "the beta row must render with its name round-tripped into data-apply");
assert.equal(listHTML.includes('data-delete="alpha"'), true, "the alpha row must render a Delete button with data-delete");
assert.equal(listHTML.indexOf("alpha") < listHTML.indexOf("beta"), true, "rows must render in the daemon's name order");

// An unchanged names array must NOT rewrite the list's innerHTML (each
// rewrite detaches the row buttons and any focus inside the card).
const settingsList = document.getElementById("settings-list");
let innerHTMLWrites = 0;
let innerHTMLBacking = settingsList.innerHTML;
Object.defineProperty(settingsList, "innerHTML", {
	get() { return innerHTMLBacking; },
	set(value) { innerHTMLWrites += 1; innerHTMLBacking = String(value); },
});
intervalCallback();
await flush();
assert.equal(innerHTMLWrites, 0, "a second refresh with identical names must not touch the list's innerHTML a second time");

stubFetchScript({
	"/api/lights": {status: 200, body: {lights: []}},
	"/api/mic": {status: 200, body: {state: "unmuted"}},
	"/api/settings": {status: 200, body: {names: ["alpha", "beta", "gamma"]}},
	"POST /api/settings": {status: 200, body: {names: ["alpha", "beta"]}},
});
intervalCallback();
await flush();
assert.equal(innerHTMLWrites, 1, "a changed names array must rewrite the list's innerHTML exactly once");
assert.equal(settingsList.innerHTML.includes('data-apply="gamma"'), true, "the new row must render after a names change");

fetchCalls.length = 0;
settingsApplyAlpha.click();
settingsDeleteAlpha.click();
settingsApplyBeta.click();
settingsDeleteBeta.click();
await flush();
let posts = fetchCalls
	.filter((call) => call.options.method === "POST")
	.map((call) => ({url: call.url, body: JSON.parse(call.options.body)}));
assert.deepEqual(posts, [
	{url: "/api/settings", body: {action: "apply", name: "alpha"}},
	{url: "/api/settings", body: {action: "delete", name: "alpha"}},
	{url: "/api/settings", body: {action: "apply", name: "beta"}},
	{url: "/api/settings", body: {action: "delete", name: "beta"}},
], "each settings row's Apply/Delete must enqueue exactly one /api/settings payload through the queue, name round-tripped verbatim, and nothing else");

fetchCalls.length = 0;
const saveInput = document.getElementById("settings-name");
const saveForm = document.getElementById("settings-form");
saveInput.value = "";
saveForm.submit();
saveInput.value = "   \t ";
saveForm.submit();
await flush();
assert.deepEqual(
	fetchCalls.filter((call) => call.options.method === "POST"),
	[],
	"Save with an empty or whitespace-only name must never issue a mutation",
);

saveInput.value = "movie mode";
saveForm.submit();
await flush();
assert.deepEqual(
	fetchCalls.filter((call) => call.options.method === "POST").map((call) => ({url: call.url, body: JSON.parse(call.options.body)})),
	[{url: "/api/settings", body: {action: "save", name: "movie mode"}}],
	"Save with a real name must enqueue exactly one save mutation with the name verbatim",
);
`)
}

// TestEmbeddedLightUISettingsSpecialNameRoundTripNodeDOMStub EXECUTES the
// embedded page script under Node against the shared hand-rolled DOM stub and
// pins the special-character name round trip: a name containing quotes,
// spaces, ampersand and angle brackets (all legal per the daemon's name
// rules) renders entity-escaped in the row label AND in both data-*
// payloads; the browser's attribute->dataset entity decode (simulated
// stub-side) returns the payload to the verbatim name; and a click on the
// row's Apply/Delete enqueues that verbatim name. It also pins the empty
// state's transitions across an in-band daemon error while the names array
// itself is unchanged.
func TestEmbeddedLightUISettingsSpecialNameRoundTripNodeDOMStub(t *testing.T) {
	runPageScriptWithDOMStub(t, `
const specialName = 'a "b" & <c> \'d\'';
stubFetchScript({
	"/api/lights": {status: 200, body: {lights: []}},
	"/api/mic": {status: 200, body: {state: "unmuted"}},
	"/api/settings": {status: 200, body: {names: [specialName]}},
	"POST /api/settings": {status: 200, body: {names: [specialName]}},
});
const specialApply = makeElement("settings-apply-special");
const specialDelete = makeElement("settings-delete-special");
stubRegister("[data-apply]", [specialApply]);
stubRegister("[data-delete]", [specialDelete]);
intervalCallback();
await flush();

const markup = document.getElementById("settings-list").innerHTML;
const escaped = "a &quot;b&quot; &amp; &lt;c&gt; &#39;d&#39;";
assert.equal(markup.includes('>' + escaped + "</button>"), true, "the row label must render the entity-escaped name");
assert.equal(markup.includes('data-apply="' + escaped + '"'), true, "the Apply button's data-apply must carry the entity-escaped name");
assert.equal(markup.includes('data-delete="' + escaped + '"'), true, "the Delete button's data-delete must carry the entity-escaped name");
assert.equal(markup.includes(specialName), false, "raw markup must never contain the unescaped name");

// The browser entity-DECODES data-* attribute values into dataset; run that
// decode stub-side and require the payload to round-trip to the verbatim name.
const attribute = markup.match(/data-apply="([^"]*)"/);
assert.ok(attribute, "the rendered row must contain a data-apply attribute");
const decoded = attribute[1].replace(/&(quot|amp|lt|gt|#39);/g, (entity, code) => ({quot: '"', amp: "&", lt: "<", gt: ">", "#39": "'"}[code]));
assert.equal(decoded, specialName, "the attribute decode must round-trip the payload back to the verbatim saved name");

fetchCalls.length = 0;
specialApply.dataset.apply = decoded;
specialDelete.dataset.delete = decoded;
specialApply.click();
specialDelete.click();
await flush();
assert.deepEqual(
	fetchCalls.filter((call) => call.options.method === "POST").map((call) => ({url: call.url, body: JSON.parse(call.options.body)})),
	[
		{url: "/api/settings", body: {action: "apply", name: specialName}},
		{url: "/api/settings", body: {action: "delete", name: specialName}},
	],
	"Apply/Delete on a special-character row must enqueue the verbatim entity-decoded name through the queue, and nothing else",
);

// Empty-state transitions with an UNCHANGED names array: the empty element
// must still track the in-band daemon error line (store-disabled regime).
stubFetchScript({
	"/api/lights": {status: 200, body: {lights: []}},
	"/api/mic": {status: 200, body: {state: "unmuted"}},
	"/api/settings": {status: 200, body: {names: [], error: "error: saved settings are disabled"}},
});
intervalCallback();
await flush();
assert.equal(document.getElementById("settings-empty").hidden, true, "an in-band daemon error must keep the empty state hidden");
stubFetchScript({
	"/api/lights": {status: 200, body: {lights: []}},
	"/api/mic": {status: 200, body: {state: "unmuted"}},
	"/api/settings": {status: 200, body: {names: []}},
});
intervalCallback();
await flush();
assert.equal(document.getElementById("settings-empty").hidden, false, "the empty state must reappear when the error clears even though the names array did not change");
`)
}

// TestEmbeddedLightUIApplyDetailBannerNodeDOMStub EXECUTES the embedded page
// script under Node against the DOM stub and pins showApplyDetail's banner
// gate against BOTH of the daemon's real failure line shapes: a detail
// carrying a label-prefixed per-light failure ("COM7: error: timeout" - the
// callLight/Manager.apply shape from the settingsApply fan-out) must raise
// the error banner with the daemon's line verbatim, as must the
// line-initial "error:" skip shape; a detail of ONLY good status lines
// ("COM4 desk: on 47% 2900K", "COM7: off") must NOT raise it - the gate can
// never misfire on a well-known good line.
func TestEmbeddedLightUIApplyDetailBannerNodeDOMStub(t *testing.T) {
	runPageScriptWithDOMStub(t, `
const banner = document.getElementById("error-banner");
const errorText = document.getElementById("error-text");

stubFetchScript({
	"/api/lights": {status: 200, body: {lights: []}},
	"/api/mic": {status: 200, body: {state: "unmuted"}},
	"/api/settings": {status: 200, body: {names: ["alpha", "beta"]}},
	"POST /api/settings": {status: 200, body: {names: ["alpha", "beta"], detail: "COM4 desk: on 47% 2900K\nCOM7: off"}},
});
intervalCallback();
await flush();
banner.hidden = true;
settingsApplyAlpha.click();
await flush();
assert.equal(banner.hidden, true, "an all-good apply detail must NOT raise the error banner");

stubFetchScript({
	"/api/lights": {status: 200, body: {lights: []}},
	"/api/mic": {status: 200, body: {state: "unmuted"}},
	"/api/settings": {status: 200, body: {names: ["alpha", "beta"]}},
	"POST /api/settings": {status: 200, body: {names: ["alpha", "beta"], detail: "COM4 desk: on 47% 2900K\nCOM7: error: timeout"}},
});
settingsApplyBeta.click();
await flush();
assert.equal(banner.hidden, false, "a detail carrying a label-prefixed per-light failure must raise the error banner (partial failure is never hidden)");
assert.equal(errorText.textContent.includes("COM7: error: timeout"), true, "the banner must carry the daemon's failure line verbatim");

banner.hidden = true;
errorText.textContent = "";
stubFetchScript({
	"/api/lights": {status: 200, body: {lights: []}},
	"/api/mic": {status: 200, body: {state: "unmuted"}},
	"/api/settings": {status: 200, body: {names: ["alpha", "beta"]}},
	"POST /api/settings": {status: 200, body: {names: ["alpha", "beta"], detail: "COM4 desk: on 47% 2900K\nerror: light \"COM9\": unreachable, skipped"}},
});
settingsApplyAlpha.click();
await flush();
assert.equal(banner.hidden, false, "a detail carrying a line-initial error: skip line must keep raising the error banner");
assert.equal(errorText.textContent.includes("error: light \"COM9\": unreachable, skipped"), true, "the banner must carry the skip line verbatim");
`)
}

// TestEmbeddedLightUIPollersHaveInFlightGuards EXECUTES the embedded page
// script under Node against the shared DOM stub and pins the in-flight guard
// on ALL THREE pollers, identical in shape to refreshLights': while one
// poll's request is still pending, back-to-back interval ticks must NOT
// stack new requests - a hung daemon parks every request on the shared
// dispatcher's 6 s timeout, so unguarded 750 ms polls would build an
// unbounded fetch backlog that also delays user mutations. Once the pending
// request completes, the next tick polls again. The fetch STUB resolves
// immediately, but each poller only clears its guard in the finally after
// its awaits - a microtask later - so three SYNCHRONOUS ticks issue exactly
// one request per endpoint when the guard holds.
func TestEmbeddedLightUIPollersHaveInFlightGuards(t *testing.T) {
	runPageScriptWithDOMStub(t, `
stubFetchScript({
	"/api/lights": {status: 200, body: {lights: []}},
	"/api/mic": {status: 200, body: {state: "unmuted"}},
	"/api/settings": {status: 200, body: {names: []}},
});
fetchCalls.length = 0;
const countGets = (url) => fetchCalls.filter((call) => call.url === url && !(call.options && call.options.method)).length;
// Three back-to-back ticks with nothing resolved in between: each poller
// must still show exactly ONE request - the second and third ticks find
// the first poll in flight and skip.
intervalCallback();
intervalCallback();
intervalCallback();
assert.equal(countGets("/api/lights"), 1, "refreshLights' in-flight guard: a pending poll issues no new /api/lights request");
assert.equal(countGets("/api/mic"), 1, "a pending refreshMic request must suppress further /api/mic polls");
assert.equal(countGets("/api/settings"), 1, "a pending refreshSettings request must suppress further /api/settings polls");
// Once the pending requests complete, the next tick polls every endpoint
// again - the guards reset, the page keeps refreshing.
await flush();
intervalCallback();
await flush();
assert.equal(countGets("/api/lights"), 2, "after completion the next /api/lights poll fires");
assert.equal(countGets("/api/mic"), 2, "after completion the next /api/mic poll fires");
assert.equal(countGets("/api/settings"), 2, "after completion the next /api/settings poll fires");
`)
}

// TestEmbeddedLightUISettingsFlushSliderDebounceNodeDOMStub EXECUTES the
// embedded page script under Node against the shared DOM stub and pins the
// R2-F4 ordering: a settings Apply or Save enqueued while a light-slider
// debounce is still pending (inside its 100 ms window) lands the user's
// LATEST slider input FIRST - the pending slider mutation executes
// immediately, its timer cleared - THEN the settings operation:
// [slider command, apply] and [slider command, save], in that order, with
// no duplicate slider mutation once the debounce window lapses.
func TestEmbeddedLightUISettingsFlushSliderDebounceNodeDOMStub(t *testing.T) {
	runPageScriptWithDOMStub(t, `
stubFetchScript({
	"/api/lights": {status: 200, body: {lights: []}},
	"/api/mic": {status: 200, body: {state: "unmuted"}},
	"/api/settings": {status: 200, body: {names: ["alpha"]}},
	"POST /api/settings": {status: 200, body: {names: ["alpha"]}},
	"POST /api/group": {status: 200, body: {}},
});
intervalCallback();
await flush();

const posts = () => fetchCalls
	.filter((call) => call.options.method === "POST")
	.map((call) => ({url: call.url, body: JSON.parse(call.options.body)}));

// Slider change, then Apply INSIDE the debounce window: the pending slider
// mutation must land BEFORE the apply.
const groupBrightness = document.getElementById("group-brightness");
groupBrightness.value = "40";
groupBrightness.dispatch("input");
fetchCalls.length = 0;
settingsApplyAlpha.click();
await flush();
assert.deepEqual(posts(), [
	{url: "/api/group", body: {action: "set-brightness", value: 40}},
	{url: "/api/settings", body: {action: "apply", name: "alpha"}},
], "a settings Apply right after a slider input must land the LATEST slider value FIRST, then the apply");

// The flushed timer was cleared: once the 100 ms debounce window lapses, no
// duplicate slider mutation may follow.
await new Promise((resolve) => setTimeout(resolve, 150));
assert.deepEqual(posts(), [
	{url: "/api/group", body: {action: "set-brightness", value: 40}},
	{url: "/api/settings", body: {action: "apply", name: "alpha"}},
], "the flushed slider debounce must not fire a duplicate once its window lapses");

// Same ordering for Save (with the temp slider this time).
const groupTemp = document.getElementById("group-temp");
groupTemp.value = "3";
groupTemp.dispatch("input");
fetchCalls.length = 0;
const saveInput = document.getElementById("settings-name");
saveInput.value = "evening";
document.getElementById("settings-form").submit();
await flush();
assert.deepEqual(posts(), [
	{url: "/api/group", body: {action: "set-temp", value: 3583}},
	{url: "/api/settings", body: {action: "save", name: "evening"}},
], "a settings Save right after a slider input must land the LATEST slider value FIRST, then the save");
await new Promise((resolve) => setTimeout(resolve, 150));
assert.deepEqual(posts(), [
	{url: "/api/group", body: {action: "set-temp", value: 3583}},
	{url: "/api/settings", body: {action: "save", name: "evening"}},
], "the flushed temp-slider debounce must not fire a duplicate once its window lapses");
`)
}

// TestEmbeddedLightUISettingsNameByteCapGateNodeDOMStub EXECUTES the
// embedded page script under Node against the shared DOM stub and pins the
// R3-F6 client-side name gate: a save or delete name whose UTF-8 BYTE
// length exceeds 42 is rejected in the page BEFORE any network call - no
// mutation request is issued and the error banner carries the daemon's own
// too-long message verbatim - while a 42-byte name (the inclusive cap)
// still sends. Byte-vs-character counting is the point: 21 CJK characters
// (63 bytes, only 21 UTF-16 code units - well under the input's maxlength)
// must be rejected too, and 14 (exactly 42 bytes) must send.
func TestEmbeddedLightUISettingsNameByteCapGateNodeDOMStub(t *testing.T) {
	runPageScriptWithDOMStub(t, `
stubFetchScript({
	"/api/lights": {status: 200, body: {lights: []}},
	"/api/mic": {status: 200, body: {state: "unmuted"}},
	"/api/settings": {status: 200, body: {names: ["look"]}},
	"POST /api/settings": {status: 200, body: {names: ["look"]}},
});
const banner = document.getElementById("error-banner");
const errorText = document.getElementById("error-text");
const posts = () => fetchCalls
	.filter((call) => call.options.method === "POST")
	.map((call) => ({url: call.url, body: JSON.parse(call.options.body)}));

// Delete buttons at the byte-cap boundary; the names change below forces
// renderSettings to rebuild and bind them (the stub does not parse the list
// innerHTML, so they are registered by selector like the mic buttons).
const deleteTooLong = makeElement("settings-delete-too-long");
deleteTooLong.dataset.delete = "d".repeat(43);
const deleteAtCap = makeElement("settings-delete-at-cap");
deleteAtCap.dataset.delete = "e".repeat(42);
stubRegister("[data-delete]", [deleteTooLong, deleteAtCap]);
intervalCallback();
await flush();
assert.equal(document.getElementById("settings-list").innerHTML.includes('data-apply="look"'), true, "setup: the names change rendered the settings list");

fetchCalls.length = 0;
banner.hidden = true;
errorText.textContent = "";

// A 43-byte Save is rejected in-page: NO mutation request, and the banner
// carries the daemon's own too-long message verbatim.
const saveInput = document.getElementById("settings-name");
const saveForm = document.getElementById("settings-form");
saveInput.value = "s".repeat(43);
saveForm.submit();
await flush();
assert.deepEqual(posts(), [], "a 43-byte save name must never issue a mutation request");
assert.equal(banner.hidden, false, "a rejected save must raise the error banner");
assert.equal(errorText.textContent, "error: settings name too long (max 42 bytes)", "the banner must carry the daemon's too-long message verbatim");
assert.equal(saveInput.value, "s".repeat(43), "a rejected save keeps the typed name (the input is not cleared)");

// Byte-vs-character counting (TextEncoder): 21 CJK characters = 63 bytes
// but only 21 UTF-16 code units - under the input's maxlength - and must
// still be rejected.
saveInput.value = "中".repeat(21);
saveForm.submit();
await flush();
assert.deepEqual(posts(), [], "a 63-byte (21-char CJK) save name must never issue a mutation request");
assert.equal(errorText.textContent, "error: settings name too long (max 42 bytes)", "the CJK rejection carries the daemon's message verbatim");

// A 43-byte row Delete is rejected identically. Clear the banner first so
// the assertions below can only come from THIS click (proving the button's
// listener is bound and took the rejection path).
banner.hidden = true;
errorText.textContent = "";
deleteTooLong.click();
await flush();
assert.deepEqual(posts(), [], "a 43-byte delete name must never issue a mutation request");
assert.equal(banner.hidden, false, "a rejected delete must raise the error banner");
assert.equal(errorText.textContent, "error: settings name too long (max 42 bytes)", "the banner must carry the daemon's too-long message verbatim for delete");

// 42 bytes - the inclusive cap - SENDS for both verbs, and the successful
// save's onSuccess clears the rejection banner again.
saveInput.value = "s".repeat(42);
saveForm.submit();
await flush();
assert.deepEqual(posts(), [{url: "/api/settings", body: {action: "save", name: "s".repeat(42)}}], "a 42-byte save name must send");
assert.equal(banner.hidden, true, "a successful save clears the rejection banner");

fetchCalls.length = 0;
deleteAtCap.click();
await flush();
assert.deepEqual(posts(), [{url: "/api/settings", body: {action: "delete", name: "e".repeat(42)}}], "a 42-byte delete name must send");

// 14 CJK characters = exactly 42 bytes: sends (the cap is BYTES, not chars).
fetchCalls.length = 0;
saveInput.value = "中".repeat(14);
saveForm.submit();
await flush();
assert.deepEqual(posts(), [{url: "/api/settings", body: {action: "save", name: "中".repeat(14)}}], "a 42-byte (14-char CJK) save name must send");
`)
}

// TestEmbeddedLightUIMicCardUsesTheMicEndpoints pins the mic card's wiring
// contract: badge/status-line ids, the three verb buttons, the queued
// mutation string, the shared 750 ms poll - and the ABSENCE of any direct
// fetch POST (mic mutations must go through the mutation queue). The Toggle
// button must also start DISABLED in the static markup: the badge renders
// unknown until the first definitive poll, and updateMic owns the flag from
// that first reply onward (Mute/Unmute are absolute verbs, so they stay
// armed). The DOM stub pre-registers its buttons and cannot observe initial
// markup, so this fragment is the pin for the initially-disabled Toggle.
func TestEmbeddedLightUIMicCardUsesTheMicEndpoints(t *testing.T) {
	source := string(lightUIHTML)
	for _, fragment := range []string{
		`id="mic-status"`,
		`id="mic-line"`,
		`id="mic-word"`,
		`data-mic-action="toggle"`,
		`data-mic-action="toggle" aria-label="Toggle microphone mute"`,
		`.status-badge[data-state="unreachable"]`,
		"enqueueMutation(`mic:${action}`, \"/api/mic\", {action}, false)",
		`function refreshMic()`,
		`function updateMic(`,
		`function bindMicControls()`,
		`window.setInterval(() => { refreshLights(true); refreshMic(); refreshSettings(); }, 750);`,
	} {
		if !strings.Contains(source, fragment) {
			t.Fatalf("embedded UI is missing mic card fragment %q", fragment)
		}
	}
	if strings.Contains(source, `fetch("/api/mic", {method: "POST"`) {
		t.Fatal("mic mutations must go through the mutation queue, never a direct fetch POST")
	}
}

// TestEmbeddedLightUIMicCardBehaviorNodeDOMStub EXECUTES the embedded page
// script under Node against the shared hand-rolled DOM stub (NOT a browser,
// no new dependencies) and pins the mic card's runtime behavior: each button
// enqueues exactly the right /api/mic payload through the mutation queue and
// nothing else; Toggle disarms at unknown/unreachable while the absolute
// Mute/Unmute verbs stay armed at unknown and only disarm at unreachable.
func TestEmbeddedLightUIMicCardBehaviorNodeDOMStub(t *testing.T) {
	runPageScriptWithDOMStub(t, `
stubFetchScript({
	"/api/lights": {status: 200, body: {lights: []}},
	"/api/mic": {status: 200, body: {state: "unknown"}},
	"POST /api/mic": {status: 200, body: {state: "muted"}},
});
intervalCallback();
await flush();
assert.equal(micToggleButton.disabled, true, "toggle must be disabled while the mic state is unknown (the daemon's toggle would resolve to an unpredictable absolute mute)");
assert.equal(micMuteButton.disabled, false, "mute must stay armed at unknown - it is an absolute verb");
assert.equal(micUnmuteButton.disabled, false, "unmute must stay armed at unknown - it is an absolute verb");

stubFetchScript({
	"/api/lights": {status: 200, body: {lights: []}},
	"/api/mic": {status: 200, body: {state: "unreachable"}},
	"POST /api/mic": {status: 200, body: {state: "unreachable"}},
});
intervalCallback();
await flush();
assert.equal(micToggleButton.disabled, true, "toggle must be disabled while the daemon is unreachable");
assert.equal(micMuteButton.disabled, true, "mute must be disabled while the daemon is unreachable");
assert.equal(micUnmuteButton.disabled, true, "unmute must be disabled while the daemon is unreachable");

for (const micState of ["muted", "unmuted"]) {
	stubFetchScript({
		"/api/lights": {status: 200, body: {lights: []}},
		"/api/mic": {status: 200, body: {state: micState}},
		"POST /api/mic": {status: 200, body: {state: micState}},
	});
	intervalCallback();
	await flush();
	assert.equal(micToggleButton.disabled, false, "toggle must be armed at " + micState);
	assert.equal(micMuteButton.disabled, false, "mute must be armed at " + micState);
	assert.equal(micUnmuteButton.disabled, false, "unmute must be armed at " + micState);
}

fetchCalls.length = 0;
micMuteButton.click();
micUnmuteButton.click();
micToggleButton.click();
await flush();
const posts = fetchCalls
	.filter((call) => call.options.method === "POST")
	.map((call) => ({url: call.url, body: JSON.parse(call.options.body)}));
assert.deepEqual(posts, [
	{url: "/api/mic", body: {action: "mute"}},
	{url: "/api/mic", body: {action: "unmute"}},
	{url: "/api/mic", body: {action: "toggle"}},
], "each mic button must enqueue exactly one /api/mic mutation through the queue, and nothing else");
`)
}

// TestEmbeddedLightUIInlineScriptCompilesNode is the syntax gate for the
// embedded page's inline IIFE: it must COMPILE under Node (a syntax error
// would silently kill the whole page in the browser). Behavioral DOM-stub
// execution lives in TestEmbeddedLightUIMicCardBehaviorNodeDOMStub.
func TestEmbeddedLightUIInlineScriptCompilesNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatalf("node is required for the inline-script syntax gate: %v", err)
	}
	dir := t.TempDir()
	pagePath := filepath.Join(dir, "inline.js")
	if err := os.WriteFile(pagePath, []byte(extractUIInlineScript(t)), 0o644); err != nil {
		t.Fatal(err)
	}
	gatePath := filepath.Join(dir, "compile_gate.js")
	gate := `"use strict";
const fs = require("node:fs");
const vm = require("node:vm");
const source = fs.readFileSync(process.argv[2], "utf8");
new vm.Script(source, {filename: "index.html-inline-script.js"});
`
	if err := os.WriteFile(gatePath, []byte(gate), 0o644); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(node, gatePath, pagePath).CombinedOutput()
	if err != nil {
		t.Fatalf("the inline IIFE must compile under Node: %v\n%s", err, output)
	}
}

// extractUIInlineScript returns the source of the embedded panel's single
// inline <script> block (the only attribute-less script tag; the mutation
// queue loads from a sibling src tag).
func extractUIInlineScript(t *testing.T) string {
	t.Helper()
	source := string(lightUIHTML)
	const openTag = "<script>"
	if got := strings.Count(source, openTag); got != 1 {
		t.Fatalf("embedded UI must contain exactly one inline %s tag; found %d", openTag, got)
	}
	rest := source[strings.Index(source, openTag)+len(openTag):]
	const closeTag = "</script>"
	end := strings.Index(rest, closeTag)
	if end < 0 {
		t.Fatal("inline script block has no closing </script>")
	}
	script := rest[:end]
	if strings.Contains(script, "<script") {
		t.Fatal("inline script extraction swallowed a nested script tag")
	}
	return script
}

// Hand-rolled minimal DOM stub for the embedded panel script, in the same
// Node harness style as TestLightMutationQueueNode. This is NOT a browser: it
// covers exactly the elements and behaviors the cards use (getElementById
// lookups, dataset flags, disabled/hidden toggles, click dispatch, scripted
// fetch replies, the shared 750 ms interval callback). The driver runs after
// the page's initial poll tick (flush drains it) and asserts behavior.
const lightUIDOMStubPreludeJS = `"use strict";
const assert = require("node:assert/strict");
const LightMutationQueue = require("./mutation_queue.js");

function makeElement(id) {
	const element = {
		id: id || "",
		dataset: {},
		hidden: false,
		disabled: false,
		value: "",
		textContent: "",
		innerHTML: "",
		listeners: Object.create(null),
		attributes: Object.create(null),
		addEventListener(type, listener) {
			(element.listeners[type] = element.listeners[type] || []).push(listener);
		},
		click() {
			(element.listeners.click || []).forEach((listener) => listener());
		},
		submit() {
			(element.listeners.submit || []).forEach((listener) => listener({preventDefault() {}}));
		},
		dispatch(type) {
			(element.listeners[type] || []).forEach((listener) => listener({preventDefault() {}}));
		},
		setAttribute(name, value) {
			element.attributes[name] = String(value);
		},
		getAttribute(name) {
			return Object.prototype.hasOwnProperty.call(element.attributes, name) ? element.attributes[name] : null;
		},
		querySelector() {
			return makeElement("");
		},
		querySelectorAll() {
			return [];
		},
	};
	return element;
}

const elementRegistry = new Map();
function elementById(id) {
	if (!elementRegistry.has(id)) {
		elementRegistry.set(id, makeElement(id));
	}
	return elementRegistry.get(id);
}
const selectorRegistry = new Map();
function stubRegister(selector, elements) {
	selectorRegistry.set(selector, elements);
}
globalThis.document = {
	getElementById: elementById,
	activeElement: null,
	querySelectorAll(selector) {
		return selectorRegistry.get(selector) || [];
	},
};

const fetchCalls = [];
let fetchScript = {};
function stubFetchScript(script) {
	fetchScript = script || {};
}
globalThis.fetch = (url, options) => {
	const method = (options && options.method) || "GET";
	const key = method === "GET" ? String(url) : method + " " + String(url);
	fetchCalls.push({url: String(url), options: options || {}});
	const scripted = Object.prototype.hasOwnProperty.call(fetchScript, key) ? fetchScript[key] : fetchScript[String(url)];
	if (scripted instanceof Error) {
		return Promise.reject(scripted);
	}
	const status = scripted && scripted.status ? scripted.status : 200;
	const body = scripted && Object.prototype.hasOwnProperty.call(scripted, "body") ? scripted.body : {};
	return Promise.resolve({ok: status >= 200 && status < 300, status: status, json: () => Promise.resolve(body)});
};

let intervalCallback = null;
globalThis.window = {
	setInterval(callback) {
		intervalCallback = callback;
	},
};

async function flush() {
	for (let round = 0; round < 20; round += 1) {
		await new Promise((resolve) => setImmediate(resolve));
	}
}

const micMuteButton = makeElement("mic-mute");
micMuteButton.dataset.micAction = "mute";
const micUnmuteButton = makeElement("mic-unmute");
micUnmuteButton.dataset.micAction = "unmute";
const micToggleButton = makeElement("mic-toggle");
micToggleButton.dataset.micAction = "toggle";
stubRegister("[data-mic-action]", [micMuteButton, micUnmuteButton, micToggleButton]);

// Pre-registered saved-settings rows (the stub has no innerHTML parsing; the
// page binds through document-level querySelectorAll like the mic buttons).
// Names are set directly on the dataset, exactly as the browser would decode
// them from the data-apply/data-delete attributes renderSettings writes.
const settingsApplyAlpha = makeElement("settings-apply-alpha");
settingsApplyAlpha.dataset.apply = "alpha";
const settingsApplyBeta = makeElement("settings-apply-beta");
settingsApplyBeta.dataset.apply = "beta";
stubRegister("[data-apply]", [settingsApplyAlpha, settingsApplyBeta]);
const settingsDeleteAlpha = makeElement("settings-delete-alpha");
settingsDeleteAlpha.dataset.delete = "alpha";
const settingsDeleteBeta = makeElement("settings-delete-beta");
settingsDeleteBeta.dataset.delete = "beta";
stubRegister("[data-delete]", [settingsDeleteAlpha, settingsDeleteBeta]);
`

// runPageScriptWithDOMStub executes the embedded page's inline script under
// Node against the hand-rolled DOM stub above, then runs the driver source.
// The real mutation_queue.js is loaded from disk so button clicks flow
// through the production queue before the fetch stub sees them.
func runPageScriptWithDOMStub(t *testing.T, driver string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatalf("node is required for the page DOM-stub behavioral tests: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mutation_queue.js"), lightMutationQueueJS, 0o644); err != nil {
		t.Fatal(err)
	}
	program := lightUIDOMStubPreludeJS + "\n" + extractUIInlineScript(t) + "\n(async () => {\n\tawait flush();\n" + driver + "\n})().catch((error) => {\n\tconsole.error(error);\n\tprocess.exitCode = 1;\n});\n"
	programPath := filepath.Join(dir, "page_behavior_test.js")
	if err := os.WriteFile(programPath, []byte(program), 0o644); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(node, programPath).CombinedOutput()
	if err != nil {
		t.Fatalf("node %s failed: %v\n%s", programPath, err, output)
	}
}
