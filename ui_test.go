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
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
	wantPorts := []string{"COM6", "COM12", "COM4", "COM5"}
	if !reflect.DeepEqual(gotPorts, wantPorts) {
		t.Fatalf("sorted ports = %v, want %v", gotPorts, wantPorts)
	}
	gotNames := make([]string, len(lights))
	for i, light := range lights {
		gotNames[i] = light.Name
	}
	wantNames := []string{"desk-left", "desk-center", "desk-right", ""}
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
	want := []string{"desk-left", "desk-center", "desk-right", ""}
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
		"desk-center@COM12",
		"desk-right@COM4",
		"alpha@COM2",
		"zeta@COM30",
		"@COM10",
		"@COM99",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sorted lights = %v, want %v", got, want)
	}
}

func TestEmbeddedLightUICardsUseTheirOwnIdentityAsTarget(t *testing.T) {
	source := string(lightUIHTML)
	for _, fragment := range []string{
		`const on = lightIsOn(light);`,
		`const brightnessDisplay = on ?`,
		`const tempDisplay = on ?`,
		`data-port="${port}"`,
		`const target = port;`,
		`{target, action: "toggle"}`,
		`{target, action: field, value: field === "brightness" ? value : TEMP_STEPS[value]}`,
		`lights.map(cardMarkup)`,
	} {
		if !strings.Contains(source, fragment) {
			t.Fatalf("embedded UI is missing identity-bound control fragment %q", fragment)
		}
	}
	for _, fragment := range []string{
		`brightness.disabled = !on;`,
		`temp.disabled = !on;`,
		`$("group-brightness-output").textContent = "Mixed";`,
		`$("group-temp-output").textContent = "Mixed";`,
		`refreshLights(true);`,
	} {
		if !strings.Contains(source, fragment) {
			t.Fatalf("embedded UI is missing state/refresh fragment %q", fragment)
		}
	}
	if strings.Contains(source, `onSettled: () => refreshLights(true)`) {
		t.Fatal("successful mutation must not trigger a redundant status refresh")
	}
	if strings.Contains(source, `const target = escapeHTML(light.name || light.port);`) || strings.Contains(source, `data-target`) {
		t.Fatal("card controls must target the canonical COM port, never the mutable display name")
	}
	if strings.Contains(source, "lights[index]") || strings.Contains(source, "data-index") {
		t.Fatal("embedded UI must not bind a card control through a visual array index")
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
		`data-mic-action="mute"`,
		`data-mic-action="unmute"`,
		`data-mic-action="toggle"`,
		`<button class="button-quiet" type="button" data-mic-action="toggle" disabled>Toggle</button>`,
		`.status-badge[data-state="unreachable"]`,
		"enqueueMutation(`mic:${action}`, \"/api/mic\", {action}, false)",
		`function refreshMic()`,
		`function updateMic(`,
		`function bindMicControls()`,
		`window.setInterval(() => { refreshLights(true); refreshMic(); }, 750);`,
	} {
		if !strings.Contains(source, fragment) {
			t.Fatalf("embedded UI is missing mic card fragment %q", fragment)
		}
	}
	for _, armed := range []string{`data-mic-action="mute" disabled`, `data-mic-action="unmute" disabled`} {
		if strings.Contains(source, armed) {
			t.Fatalf("Mute/Unmute are absolute verbs and must start armed: found %q in the markup", armed)
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
