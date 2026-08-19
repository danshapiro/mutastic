package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	uiDefaultPort        = 42815
	uiBodyLimit          = 16 << 10
	uiReadHeaderTimeout  = 5 * time.Second
	uiIdleTimeout        = 60 * time.Second
	uiMaxHeaderBytes     = 16 << 10
	uiFrameAncestorsNone = "frame-ancestors 'none'"
)

// These are the PL81's discrete temperature values, in warm-to-cool order.
// The UI sends the actual Kelvin value; the daemon performs the same
// quantization as the hardware and returns the resulting value in status.
var uiTemperatureSteps = []int{
	2900, 3128, 3356, 3583, 3811,
	4039, 4267, 4494, 4722, 4950,
	5178, 5406, 5633, 5861, 6089,
	6317, 6544, 6772, 7000,
}

var (
	uiPortPattern    = regexp.MustCompile(`(?i)^COM[0-9]+$`)
	uiNamePattern    = regexp.MustCompile(`(?i)^[a-z][a-z0-9-]{0,15}$`)
	uiPercentPattern = regexp.MustCompile(`^[0-9]+%$`)
	uiKelvinPattern  = regexp.MustCompile(`^[0-9]+K$`)
)

//go:embed internal/lightui/index.html
var lightUIHTML []byte

//go:embed internal/lightui/mutation_queue.js
var lightMutationQueueJS []byte

// uiLight is the stable JSON shape consumed by the embedded controller.
// Brightness and Temp are null when the daemon cannot know those values
// (for example, an off or freshly restarted light).
type uiLight struct {
	Port       string `json:"port"`
	Name       string `json:"name"`
	Connected  bool   `json:"connected"`
	State      string `json:"state"`
	Brightness *int   `json:"brightness"`
	Temp       *int   `json:"temp"`
	Error      string `json:"error"`
}

type uiDetail struct {
	Target string `json:"target"`
	Error  string `json:"error"`
}

type uiResponse struct {
	Lights  []uiLight  `json:"lights"`
	Error   string     `json:"error,omitempty"`
	Details []uiDetail `json:"details,omitempty"`
}

type uiActionRequest struct {
	Target string `json:"target"`
	Action string `json:"action"`
	Value  *int   `json:"value"`
}

// uiSettingsResponse is the JSON shape of every /api/settings reply. Names
// is a REAL array (never null) in every reply that produced one: GET always
// lists (an empty store lists []), and POST lists on a successful follow-up
// refresh. Only the F6 path - the mutation COMMITTED but the follow-up
// settings list refresh failed - leaves Names nil so it marshals as null
// beside the refresh-failure error; a 502 there would misreport a mutation
// that already happened. Detail carries a successful apply's full verbatim
// daemon reply so the page's error banner can show partial-success skip
// lines; it is never hidden.
type uiSettingsResponse struct {
	Names  []string `json:"names"`
	Error  string   `json:"error,omitempty"`
	Detail string   `json:"detail,omitempty"`
}

type uiSettingsRequest struct {
	Action string `json:"action"`
	Name   string `json:"name"`
}

// uiMaxSettingsNameBytes mirrors the daemon store's maxSettingsNameLen in
// internal/light/settings.go (42: with the 22-byte longest verb prefix,
// "light settings delete ", the largest legal wire command is 64 bytes -
// the daemon's receive buffer is 128 with 2x headroom, R7-F3, and a
// read filling it is refused without dispatch, R8-F1). R4-F3:
// the UI enforces the SAME byte cap server-side on every settings action
// BEFORE any daemon call - the page's JS gate (R3-F6) is bypassable by a
// direct caller. Since R7-F3 an over-cap name of 65-127 bytes arrives
// whole and draws the daemon's documented rejection on every platform;
// since R8-F1 a datagram that FILLS or exceeds the buffer is refused with
// "error: command too long" on Unix (truncated to the full buffer, never
// dispatched) and still dies unanswered on Windows (WSAEMSGSIZE,
// surfacing as a timeout); this gate remains the defense that skips the
// pointless round trip in the first place. The daemon's own
// identical check stays authoritative; the two constants must stay equal.
const uiMaxSettingsNameBytes = 42

// daemonCall is deliberately tiny: tests can provide a scripted fake while
// production uses the existing askDaemon UDP client.
type daemonCall func(command string) (string, error)

// daemonDispatcher serializes each UI request's daemon round trip. Fleet
// relative changes are atomic daemon commands; this mutex only prevents
// browser requests from unnecessarily interleaving their own snapshots.
type daemonDispatcher struct {
	mu        sync.Mutex
	roundTrip daemonCall
}

func newDaemonDispatcher() *daemonDispatcher {
	return &daemonDispatcher{
		// Every UI->daemon call - the mic verbs AND the light verbs - uses
		// the single lightClientTimeout-class budget (the LB-3 ruling;
		// tray_windows.go already uses it for every ask for the same
		// reason): the daemon's serveUDP loop is strictly serial and a
		// wedged light call occupies ~2 s, so a thinner mic-specific budget
		// would flap the mic card to "unreachable" mid light-operation and
		// could report failure for mutes the daemon still dequeued and
		// executed. NO mic/light timeout split is introduced.
		roundTrip: func(command string) (string, error) {
			return askDaemon(command, udpAddr, lightClientTimeout)
		},
	}
}

func newTestDaemonDispatcher(roundTrip daemonCall) *daemonDispatcher {
	return &daemonDispatcher{roundTrip: roundTrip}
}

func (d *daemonDispatcher) sequence(fn func(daemonCall) error) error {
	if d == nil || d.roundTrip == nil {
		return errors.New("daemon dispatcher is not configured")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return fn(d.roundTrip)
}

type uiServer struct {
	dispatcher *daemonDispatcher
	port       int
	// snapNow captures one OBS frame to disk on demand; runUI wires it.
	// Nil: /api/snapshot answers 503.
	snapNow func() (uiSnapshot, error)
	// shutdown gracefully stops the owning http.Server after the reply
	// flushes; runUI wires it. Nil: /api/shutdown answers 503.
	shutdown func()
	// shutdownOnce makes repeated /api/shutdown requests safe: only the first spawns the drain goroutine (closing drainDone twice would panic).
	shutdownOnce sync.Once
}

func newUIServer(port int, dispatcher *daemonDispatcher) *uiServer {
	if dispatcher == nil {
		dispatcher = newDaemonDispatcher()
	}
	return &uiServer{dispatcher: dispatcher, port: port}
}

func (s *uiServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setUISecurityHeaders(w)
	if isCrossSiteRequest(r) {
		writeUIJSON(w, http.StatusForbidden, uiResponse{Error: "cross-site requests are not allowed"})
		return
	}
	if !s.allowedHost(r.Host) {
		writeUIJSON(w, http.StatusForbidden, uiResponse{Error: "host is not allowed"})
		return
	}

	switch r.URL.Path {
	case "/":
		if r.Method != http.MethodGet {
			writeUIMethodError(w, http.MethodGet)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(lightUIHTML)
	case "/mutation_queue.js":
		if r.Method != http.MethodGet {
			writeUIMethodError(w, http.MethodGet)
			return
		}
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(lightMutationQueueJS)
	case "/api/health":
		if r.Method != http.MethodGet {
			writeUIMethodError(w, http.MethodGet)
			return
		}
		writeUIJSON(w, http.StatusOK, struct {
			OK bool `json:"ok"`
		}{OK: true})
	case "/api/lights":
		if r.Method != http.MethodGet {
			writeUIMethodError(w, http.MethodGet)
			return
		}
		s.handleLights(w)
	case "/api/light":
		if r.Method != http.MethodPost {
			writeUIMethodError(w, http.MethodPost)
			return
		}
		if !s.validPostOrigin(r) {
			writeUIJSON(w, http.StatusForbidden, uiResponse{Error: "origin is not allowed"})
			return
		}
		s.handleLight(w, r)
	case "/api/group":
		if r.Method != http.MethodPost {
			writeUIMethodError(w, http.MethodPost)
			return
		}
		if !s.validPostOrigin(r) {
			writeUIJSON(w, http.StatusForbidden, uiResponse{Error: "origin is not allowed"})
			return
		}
		s.handleGroup(w, r)
	case "/api/settings":
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			writeUIMethodError(w, http.MethodGet+", "+http.MethodPost)
			return
		}
		if r.Method == http.MethodPost {
			if !s.validPostOrigin(r) {
				writeUIJSON(w, http.StatusForbidden, uiResponse{Error: "origin is not allowed"})
				return
			}
			s.handleSettings(w, r)
			return
		}
		s.handleSettingsList(w)
	case "/api/mic":
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			writeUIMethodError(w, http.MethodGet+", "+http.MethodPost)
			return
		}
		if r.Method == http.MethodPost {
			if !s.validPostOrigin(r) {
				writeUIJSON(w, http.StatusForbidden, uiResponse{Error: "origin is not allowed"})
				return
			}
			s.handleMic(w, r)
			return
		}
		s.handleMicStatus(w)
	case "/api/snapshot":
		if r.Method != http.MethodPost {
			writeUIMethodError(w, http.MethodPost)
			return
		}
		if !s.validPostOrigin(r) {
			writeUIJSON(w, http.StatusForbidden, uiResponse{Error: "origin is not allowed"})
			return
		}
		s.handleSnapshot(w)
	case "/api/shutdown":
		if r.Method != http.MethodPost {
			writeUIMethodError(w, http.MethodPost)
			return
		}
		if !s.validPostOrigin(r) {
			writeUIJSON(w, http.StatusForbidden, uiResponse{Error: "origin is not allowed"})
			return
		}
		if s.shutdown == nil {
			writeUIJSON(w, http.StatusServiceUnavailable, uiResponse{Error: "shutdown not wired"})
			return
		}
		writeUIJSON(w, http.StatusOK, struct {
			OK bool `json:"ok"`
		}{OK: true})
		s.shutdownOnce.Do(s.shutdown)
	default:
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeUIJSON(w, http.StatusNotFound, uiResponse{Error: "not found"})
			return
		}
		http.NotFound(w, r)
	}
}

func setUISecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", uiFrameAncestorsNone)
	w.Header().Set("X-Frame-Options", "DENY")
}

func isCrossSiteRequest(r *http.Request) bool {
	for _, value := range r.Header.Values("Sec-Fetch-Site") {
		for _, site := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(site), "cross-site") {
				return true
			}
		}
	}
	return false
}

func (s *uiServer) allowedHost(host string) bool {
	name, port, err := net.SplitHostPort(host)
	if err != nil || port != strconv.Itoa(s.port) {
		return false
	}
	return name == "127.0.0.1" || strings.EqualFold(name, "localhost")
}

func (s *uiServer) validPostOrigin(r *http.Request) bool {
	origins := r.Header.Values("Origin")
	if len(origins) > 1 {
		return false
	}
	if len(origins) == 0 || origins[0] == "" {
		return true
	}
	return origins[0] == "http://"+r.Host
}

func (s *uiServer) handleLights(w http.ResponseWriter) {
	var (
		lights  []uiLight
		details []uiDetail
	)
	err := s.dispatcher.sequence(func(call daemonCall) error {
		var fatal error
		lights, details, fatal = queryUILights(call)
		return fatal
	})
	if err != nil {
		details = append(details, uiDetail{Target: "daemon", Error: err.Error()})
	}
	if len(details) > 0 {
		writeUIJSON(w, http.StatusBadGateway, uiResponse{
			Lights:  lights,
			Error:   "light status could not be read completely",
			Details: details,
		})
		return
	}
	writeUIJSON(w, http.StatusOK, uiResponse{Lights: lights})
}

// uiMicStatus is the JSON shape of both /api/mic replies. GET always answers
// state (the daemon's failure modes collapse to "unreachable", so there is
// no in-band error field there); POST answers state after a fresh query, or
// error on a failed verb.
type uiMicStatus struct {
	State string `json:"state,omitempty"`
	Error string `json:"error,omitempty"`
}

type uiMicRequest struct {
	Action string `json:"action"`
}

func (s *uiServer) handleMicStatus(w http.ResponseWriter) {
	state := "unreachable"
	_ = s.dispatcher.sequence(func(call daemonCall) error {
		reply, err := call("status")
		if err != nil {
			return nil
		}
		state = parseUIMicState(reply)
		return nil
	})
	writeUIJSON(w, http.StatusOK, uiMicStatus{State: state})
}

func (s *uiServer) handleMic(w http.ResponseWriter, r *http.Request) {
	var req uiMicRequest
	if err := decodeUIJSON(w, r, &req); err != nil {
		writeUIJSON(w, http.StatusBadRequest, uiResponse{Error: err.Error()})
		return
	}
	switch req.Action {
	case "mute", "unmute", "toggle":
	default:
		// A bad action never reaches the daemon.
		writeUIJSON(w, http.StatusBadRequest, uiResponse{Error: fmt.Sprintf("unknown mic action %q", req.Action)})
		return
	}
	var (
		state   = "unreachable"
		failure error
	)
	_ = s.dispatcher.sequence(func(call daemonCall) error {
		reply, err := call(req.Action)
		if err != nil {
			failure = err
			return nil
		}
		// F6: a verb failure MUST surface as 502, never as a 200 the
		// mutation queue would treat as success (and use to clear the error
		// banner) for an audio action that never happened. The failure
		// shape is exactly one line starting with "error:"; on that path
		// the fresh status query is skipped.
		if trimmed := strings.TrimSpace(reply); !strings.Contains(trimmed, "\n") && strings.HasPrefix(trimmed, "error:") {
			failure = errors.New(trimmed)
			return nil
		}
		// Verb accepted: re-query rather than trust the verb's ack, so the
		// card reflects the daemon's tracked state.
		status, err := call("status")
		if err != nil {
			state = "unreachable"
			return nil
		}
		state = parseUIMicState(status)
		return nil
	})
	if failure != nil {
		writeUIJSON(w, http.StatusBadGateway, uiMicStatus{Error: failure.Error()})
		return
	}
	writeUIJSON(w, http.StatusOK, uiMicStatus{State: state})
}

// uiSnapshot is the JSON reply of POST /api/snapshot: where the frame
// landed on disk, its size, and the OBS scene that produced it.
type uiSnapshot struct {
	Path   string `json:"path"`
	Bytes  int    `json:"bytes"`
	Source string `json:"source"`
}

// handleSnapshot answers POST /api/snapshot by capturing one OBS frame.
// OBS-side failures (OBS not running, obs-websocket auth missing, scene
// without video) belong to the upstream dependency, not the panel: 502,
// the same posture as the mic verbs.
func (s *uiServer) handleSnapshot(w http.ResponseWriter) {
	if s.snapNow == nil {
		writeUIJSON(w, http.StatusServiceUnavailable, uiResponse{Error: "snapshot not wired"})
		return
	}
	snap, err := s.snapNow()
	if err != nil {
		writeUIJSON(w, http.StatusBadGateway, uiResponse{Error: err.Error()})
		return
	}
	writeUIJSON(w, http.StatusOK, snap)
}

// parseUIMicState maps the daemon's tri-state mic reply onto the UI's state
// vocabulary. Every non-state reply - including the daemon's one-line
// "error:" refusal - collapses to "unreachable", the card's only honest
// answer for "we could not learn the mic state".
func parseUIMicState(reply string) string {
	switch strings.TrimSpace(reply) {
	case "muted", "unmuted", "unknown":
		return strings.TrimSpace(reply)
	default:
		return "unreachable"
	}
}

func (s *uiServer) handleLight(w http.ResponseWriter, r *http.Request) {
	var req uiActionRequest
	if err := decodeUIJSON(w, r, &req); err != nil {
		writeUIJSON(w, http.StatusBadRequest, uiResponse{Error: err.Error()})
		return
	}
	command, err := buildLightCommand(req)
	if err != nil {
		writeUIJSON(w, http.StatusBadRequest, uiResponse{Error: err.Error()})
		return
	}
	status, response := s.mutate(func(call daemonCall) ([]uiDetail, error) {
		return executeUICommand(call, command, req.Target), nil
	})
	writeUIJSON(w, status, response)
}

func (s *uiServer) handleGroup(w http.ResponseWriter, r *http.Request) {
	var req uiActionRequest
	if err := decodeUIJSON(w, r, &req); err != nil {
		writeUIJSON(w, http.StatusBadRequest, uiResponse{Error: err.Error()})
		return
	}
	plan, err := buildGroupPlan(req)
	if err != nil {
		writeUIJSON(w, http.StatusBadRequest, uiResponse{Error: err.Error()})
		return
	}
	status, response := s.mutate(plan)
	writeUIJSON(w, status, response)
}

// handleSettingsList answers GET /api/settings. It ALWAYS answers HTTP 200
// (F5): the light polling loop already owns the page's error banner, so the
// settings list degrades in band - a daemon transport failure collapses to
// the bare word "unreachable" (the same convention as the mic card) while a
// daemon single-line "error:" refusal (a disabled or corrupt store) is
// carried through verbatim. Names is a real array in every reply.
func (s *uiServer) handleSettingsList(w http.ResponseWriter) {
	response := uiSettingsResponse{Names: []string{}}
	_ = s.dispatcher.sequence(func(call daemonCall) error {
		names, err := queryUISettings(call)
		if err != nil {
			if errors.Is(err, errUIDaemonUnreachable) {
				response.Error = "unreachable"
			} else {
				response.Error = err.Error()
			}
			return nil
		}
		response.Names = names
		return nil
	})
	writeUIJSON(w, http.StatusOK, response)
}

func (s *uiServer) handleSettings(w http.ResponseWriter, r *http.Request) {
	var req uiSettingsRequest
	// R11-F3: the RAW bounded body is UTF-8-gated BEFORE the decoder (the
	// decoder's U+FFFD coercion would otherwise smuggle a raw invalid-UTF-8
	// name past the daemon's utf8.ValidString check as a DISTINCT name).
	// The gate covers every settings action identically - the refusal
	// happens before the action switch, so save/apply/delete all get the
	// same 400 with zero daemon calls.
	if err := decodeUISettingsJSON(w, r, &req); err != nil {
		writeUIJSON(w, http.StatusBadRequest, uiResponse{Error: err.Error()})
		return
	}
	switch req.Action {
	case "save", "apply", "delete":
	default:
		// A bad action never reaches the daemon.
		writeUIJSON(w, http.StatusBadRequest, uiResponse{Error: fmt.Sprintf("unknown settings action %q", req.Action)})
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeUIJSON(w, http.StatusBadRequest, uiResponse{Error: "settings name is required"})
		return
	}
	if strings.ContainsAny(req.Name, "\r\n") {
		writeUIJSON(w, http.StatusBadRequest, uiResponse{Error: "settings name must not contain a newline"})
		return
	}
	// R4-F3: BYTE length - len on a Go string counts UTF-8 bytes, exactly
	// the daemon's own measure - and the gate sits before any daemon call.
	if len(req.Name) > uiMaxSettingsNameBytes {
		writeUIJSON(w, http.StatusBadRequest, uiResponse{Error: fmt.Sprintf("settings name too long (max %d bytes)", uiMaxSettingsNameBytes)})
		return
	}
	command := buildSettingsCommand(req.Action, req.Name)
	var (
		names       []string
		detail      string
		mutationErr error
		refreshErr  error
	)
	_ = s.dispatcher.sequence(func(call daemonCall) error {
		reply, err := call(command)
		if err != nil {
			mutationErr = err
			return nil
		}
		trimmed := strings.TrimSpace(reply)
		if req.Action == "apply" {
			// LB-4 ruling: apply replies are classified LINE-WISE. Each
			// "error:"-prefixed line is a failure (e.g. an unreachable key,
			// skipped) and every other non-blank line is a success. ZERO
			// success lines means nothing was restored - the only apply
			// failure shape, answered 502 with the verbatim reply.
			if countUIApplySuccesses(trimmed) == 0 {
				mutationErr = errors.New(trimmed)
				return nil
			}
			// >=1 success: HTTP 200 with the FULL verbatim reply as detail,
			// so the page's error banner shows per-light skip lines (a
			// partial success is never hidden).
			detail = trimmed
		} else if !strings.Contains(trimmed, "\n") && strings.HasPrefix(trimmed, "error:") {
			// save/delete fail whole-reply: exactly one "error:" line (the
			// same shape as the mic verbs; multi-line noise is not a failure).
			mutationErr = errors.New(trimmed)
			return nil
		}
		// The mutation committed. Exactly ONE settings list refresh follows;
		// its failure is NEVER a 502 (F6 - the mutation already happened).
		names, refreshErr = queryUISettings(call)
		return nil
	})
	if mutationErr != nil {
		writeUIJSON(w, http.StatusBadGateway, uiSettingsResponse{Names: []string{}, Error: mutationErr.Error()})
		return
	}
	if refreshErr != nil {
		// F6: 200 with names null/omitted PLUS an error naming the refresh
		// failure. The page's shared 750 ms poll recovers the list shortly
		// afterward, and retrying the mutation is safe/idempotent by name -
		// a retried delete surfaces the daemon's "error: unknown setting".
		writeUIJSON(w, http.StatusOK, uiSettingsResponse{
			Error:  "settings list refresh failed: " + refreshErr.Error(),
			Detail: detail,
		})
		return
	}
	writeUIJSON(w, http.StatusOK, uiSettingsResponse{Names: names, Detail: detail})
}

// buildSettingsCommand renders one daemon store verb. The name passes
// VERBATIM (spaces included): the daemon reads the raw suffix after the
// sub-verb and trims there, and the UI has already rejected empty,
// whitespace-only, and newline-bearing names before this point.
func buildSettingsCommand(action, name string) string {
	return "light settings " + action + " " + name
}

// errUIDaemonUnreachable marks a daemon TRANSPORT failure inside the
// settings queries. GET /api/settings collapses it to the bare word
// "unreachable" (the card's honest word for a dead daemon); the POST
// refresh path keeps the wrapped transport detail inside its
// refresh-failure message.
var errUIDaemonUnreachable = errors.New("unreachable")

func queryUISettings(call daemonCall) ([]string, error) {
	reply, err := call("light settings list")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errUIDaemonUnreachable, err)
	}
	return parseUISettingsNames(reply)
}

// parseUISettingsNames parses the "light settings list" reply:
// newline-joined names, "" for an empty store (a REAL empty slice, never
// nil), or a single-line "error:" refusal (disabled/corrupt store) whose
// string is returned verbatim as the error.
func parseUISettingsNames(reply string) ([]string, error) {
	trimmed := strings.TrimSpace(reply)
	if trimmed == "" {
		return []string{}, nil
	}
	if !strings.Contains(trimmed, "\n") && strings.HasPrefix(trimmed, "error:") {
		return nil, errors.New(trimmed)
	}
	names := []string{}
	for _, line := range strings.Split(trimmed, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

// countUIApplySuccesses classifies an apply reply LINE-WISE (LB-4): failure
// lines (isUIApplyFailureLine) never count; every other non-blank line is a
// successfully restored light. Zero successes means the apply restored
// nothing at all and the whole reply is reported verbatim as the failure.
func countUIApplySuccesses(reply string) int {
	successes := 0
	for _, line := range strings.Split(reply, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || isUIApplyFailureLine(line) {
			continue
		}
		successes++
	}
	return successes
}

// isUIApplyFailureLine reports whether ONE trimmed apply-reply line is a
// per-light failure. The daemon's settingsApply fan-out emits exactly two
// failure shapes: a line-initial "error:" (whole-reply refusals such as
// `error: unknown setting "x"`, or unreachable-key skips
// `error: light "COM9": unreachable, skipped`) and the label-prefixed
// per-light hardware failure `COM7: error: timeout` (callLight's deadline or
// Manager.apply's write failure rendered as mm.label(key) + ": " + reply).
// The ": error:" substring convention is the same one parseUICommandErrors
// already uses for the fleet endpoints. The predicate cannot misfire on the
// well-known good line shapes: a success line is label + ": " + StatusString
// ("on 47% 2900K" / "off" / "unknown" - no colon anywhere in the status),
// the label is a COM port plus an [a-z0-9-] registry name (so the label
// itself can never contain ":"), and this classifier only ever sees APPLY
// replies - no user-chosen settings name appears in one, and a daemon
// mutation reply like `saved "x" (2 lights)` never contains ": error:" for
// any ordinary name.
func isUIApplyFailureLine(line string) bool {
	return strings.HasPrefix(line, "error:") || strings.Contains(line, ": error:")
}

type uiMutationPlan func(call daemonCall) ([]uiDetail, error)

func (s *uiServer) mutate(plan uiMutationPlan) (int, uiResponse) {
	var (
		lights  []uiLight
		details []uiDetail
	)
	sequenceErr := s.dispatcher.sequence(func(call daemonCall) error {
		planDetails, planErr := plan(call)
		details = append(details, planDetails...)
		if planErr != nil && len(planDetails) == 0 {
			details = append(details, uiDetail{Target: "group", Error: planErr.Error()})
		}
		var snapshotDetails []uiDetail
		lights, snapshotDetails, _ = queryUILights(call)
		details = append(details, snapshotDetails...)
		return nil
	})
	if sequenceErr != nil {
		details = append(details, uiDetail{Target: "daemon", Error: sequenceErr.Error()})
	}
	response := uiResponse{Lights: lights}
	if len(details) > 0 {
		response.Error = "one or more light operations failed"
		response.Details = details
		return http.StatusBadGateway, response
	}
	return http.StatusOK, response
}

func buildLightCommand(req uiActionRequest) (string, error) {
	target, err := validateUITarget(req.Target)
	if err != nil {
		return "", err
	}
	switch req.Action {
	case "on", "off", "toggle":
		if req.Value != nil {
			return "", fmt.Errorf("value is not used for %s", req.Action)
		}
		return "light@" + target + " " + req.Action, nil
	case "brightness":
		value, err := requiredUIValue(req.Value, "brightness")
		if err != nil {
			return "", err
		}
		if value < 0 || value > 100 {
			return "", errors.New("brightness must be between 0 and 100")
		}
		return fmt.Sprintf("light@%s brightness %d", target, value), nil
	case "temp":
		value, err := requiredUIValue(req.Value, "temperature")
		if err != nil {
			return "", err
		}
		if !isUITemperature(value) {
			return "", fmt.Errorf("temperature must be one of %v", uiTemperatureSteps)
		}
		return fmt.Sprintf("light@%s temp %d", target, value), nil
	default:
		return "", fmt.Errorf("unknown light action %q", req.Action)
	}
}

func buildGroupPlan(req uiActionRequest) (uiMutationPlan, error) {
	switch req.Action {
	case "on", "off", "toggle":
		if req.Value != nil {
			return nil, fmt.Errorf("value is not used for %s", req.Action)
		}
		command := "light " + req.Action
		return func(call daemonCall) ([]uiDetail, error) {
			return executeUICommand(call, command, "all lights"), nil
		}, nil
	case "set-brightness":
		value, err := requiredUIValue(req.Value, "brightness")
		if err != nil {
			return nil, err
		}
		if value < 0 || value > 100 {
			return nil, errors.New("brightness must be between 0 and 100")
		}
		command := fmt.Sprintf("light brightness %d", value)
		return func(call daemonCall) ([]uiDetail, error) {
			return executeUICommand(call, command, "all lights"), nil
		}, nil
	case "set-temp":
		value, err := requiredUIValue(req.Value, "temperature")
		if err != nil {
			return nil, err
		}
		if !isUITemperature(value) {
			return nil, fmt.Errorf("temperature must be one of %v", uiTemperatureSteps)
		}
		command := fmt.Sprintf("light temp %d", value)
		return func(call daemonCall) ([]uiDetail, error) {
			return executeUICommand(call, command, "all lights"), nil
		}, nil
	case "brightness-delta":
		value, err := requiredUIValue(req.Value, "brightness delta")
		if err != nil {
			return nil, err
		}
		if value < -20 || value > 20 {
			return nil, errors.New("brightness delta must be between -20 and 20")
		}
		command := fmt.Sprintf("light brightness-delta %d", value)
		return func(call daemonCall) ([]uiDetail, error) {
			return executeUICommand(call, command, "all lights"), nil
		}, nil
	case "temp-step-delta":
		value, err := requiredUIValue(req.Value, "temperature step delta")
		if err != nil {
			return nil, err
		}
		if value < -3 || value > 3 {
			return nil, errors.New("temperature step delta must be between -3 and 3")
		}
		command := fmt.Sprintf("light temp-step-delta %d", value)
		return func(call daemonCall) ([]uiDetail, error) {
			return executeUICommand(call, command, "all lights"), nil
		}, nil
	default:
		return nil, fmt.Errorf("unknown group action %q", req.Action)
	}
}

func executeUICommand(call daemonCall, command, fallbackTarget string) []uiDetail {
	reply, err := call(command)
	if err != nil {
		return []uiDetail{{Target: fallbackTarget, Error: err.Error()}}
	}
	return parseUICommandErrors(reply, fallbackTarget)
}

func queryUILights(call daemonCall) ([]uiLight, []uiDetail, error) {
	// This is the 750 ms live path. "light status" reads each Manager's
	// tracked State only; it never probes the serial ports. The topology-aware
	// "light list" command remains available to the CLI, but must never be
	// placed here because it can wait on a wedged panel.
	reply, err := call("light status")
	if err != nil {
		return nil, []uiDetail{{Target: "light status", Error: err.Error()}}, err
	}
	if strings.HasPrefix(strings.TrimSpace(reply), "error:") {
		err := errors.New(strings.TrimSpace(reply))
		return nil, []uiDetail{{Target: "light status", Error: err.Error()}}, err
	}
	lights, details := parseUILightStatus(reply)
	if lights == nil {
		lights = []uiLight{}
	}
	return lights, details, nil
}

// parseUILightStatus parses the cheap multi-line "light status" reply:
// "<port> [name]: <state>". Unlike "light list", status intentionally only
// contains currently tracked sessions; disconnected named lights are supplied
// by the slower topology command and are not fabricated in this live view.
func parseUILightStatus(reply string) ([]uiLight, []uiDetail) {
	reply = strings.TrimSpace(reply)
	if reply == "" || reply == "no lights known" {
		return nil, nil
	}
	var (
		lights  []uiLight
		details []uiDetail
	)
	for _, rawLine := range strings.Split(reply, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon <= 0 {
			details = append(details, uiDetail{Target: line, Error: "malformed light status line"})
			continue
		}
		target := strings.TrimSpace(line[:colon])
		fields := strings.Fields(target)
		if len(fields) < 1 || len(fields) > 2 {
			details = append(details, uiDetail{Target: target, Error: "malformed light status target"})
			continue
		}
		port, err := validateUIPort(fields[0])
		if err != nil {
			details = append(details, uiDetail{Target: fields[0], Error: err.Error()})
			continue
		}
		name := ""
		if len(fields) == 2 {
			name = fields[1]
		}
		light := uiLight{
			Port:      port,
			Name:      name,
			Connected: true,
			State:     "unknown",
		}
		parseUIConnectedStatus(&light, strings.Fields(strings.TrimSpace(line[colon+1:])))
		if light.State == "error" {
			details = append(details, uiDetail{Target: port, Error: light.Error})
		}
		if light.State == "" {
			light.State = "error"
			light.Error = "malformed light status"
			details = append(details, uiDetail{Target: port, Error: light.Error})
		}
		lights = append(lights, light)
	}
	sortUILights(lights)
	return lights, details
}

func parseUIConnectedStatus(light *uiLight, fields []string) {
	if len(fields) == 1 {
		switch fields[0] {
		case "off":
			light.State = "off"
			return
		case "unknown":
			light.State = "unknown"
			return
		}
	}
	if len(fields) == 3 && fields[0] == "on" {
		if !uiPercentPattern.MatchString(fields[1]) || !uiKelvinPattern.MatchString(fields[2]) {
			light.State = "error"
			light.Error = "malformed on status"
			return
		}
		brightness, err := strconv.Atoi(strings.TrimSuffix(fields[1], "%"))
		if err != nil || brightness < 0 || brightness > 100 {
			light.State = "error"
			light.Error = "invalid brightness in on status"
			return
		}
		temp, err := strconv.Atoi(strings.TrimSuffix(fields[2], "K"))
		if err != nil || !isUITemperature(temp) {
			light.State = "error"
			light.Error = "invalid temperature in on status"
			return
		}
		light.State = "on"
		light.Brightness = intPointer(brightness)
		light.Temp = intPointer(temp)
		return
	}
	if len(fields) > 0 && (fields[0] == "error:" || strings.HasPrefix(fields[0], "error:")) {
		light.State = "error"
		light.Error = strings.TrimSpace(strings.Join(fields, " "))
		if light.Error == "error:" {
			light.Error = "light reported an error"
		}
		return
	}
	light.State = "error"
	light.Error = "malformed connected status"
}

func parseUICommandErrors(reply, fallbackTarget string) []uiDetail {
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return []uiDetail{{Target: fallbackTarget, Error: "empty daemon reply"}}
	}
	if strings.HasPrefix(reply, "error:") {
		return []uiDetail{{Target: fallbackTarget, Error: reply}}
	}
	var details []uiDetail
	for _, rawLine := range strings.Split(reply, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "error:") {
			details = append(details, uiDetail{Target: fallbackTarget, Error: line})
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		target := strings.TrimSpace(line[:colon])
		status := strings.TrimSpace(line[colon+1:])
		if strings.HasPrefix(status, "error:") {
			details = append(details, uiDetail{Target: target, Error: status})
		}
	}
	return details
}

func sortUILights(lights []uiLight) {
	sort.SliceStable(lights, func(i, j int) bool {
		leftRank := uiLightSortRank(lights[i].Name)
		rightRank := uiLightSortRank(lights[j].Name)
		if leftRank != rightRank {
			return leftRank < rightRank
		}

		leftName := strings.ToLower(strings.TrimSpace(lights[i].Name))
		rightName := strings.ToLower(strings.TrimSpace(lights[j].Name))
		if leftRank == uiUnknownNamedLightRank && leftName != rightName {
			return leftName < rightName
		}
		return uiPortLess(lights[i].Port, lights[j].Port)
	})
}

const (
	uiUnknownNamedLightRank = 3
	uiUnnamedLightRank      = 4
)

func uiLightSortRank(name string) int {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "desk-left":
		return 0
	case "desk-right":
		return 1
	case "desk-center":
		return 2
	case "":
		return uiUnnamedLightRank
	default:
		return uiUnknownNamedLightRank
	}
}

func uiPortLess(left, right string) bool {
	leftNumber, leftOK := uiPortNumber(left)
	rightNumber, rightOK := uiPortNumber(right)
	if leftOK && rightOK && leftNumber != rightNumber {
		return leftNumber < rightNumber
	}
	if leftOK != rightOK {
		return leftOK
	}
	return strings.ToUpper(left) < strings.ToUpper(right)
}

func uiPortNumber(port string) (int, bool) {
	port = strings.ToUpper(port)
	if !strings.HasPrefix(port, "COM") {
		return 0, false
	}
	number, err := strconv.Atoi(strings.TrimPrefix(port, "COM"))
	if err != nil {
		return 0, false
	}
	return number, true
}

func validateUITarget(target string) (string, error) {
	if target == "" || target != strings.TrimSpace(target) {
		return "", errors.New("target must be a light name or COM port")
	}
	if port, err := validateUIPort(target); err == nil {
		return port, nil
	}
	if !uiNamePattern.MatchString(target) {
		return "", errors.New("target must be a light name or COM port")
	}
	return strings.ToLower(target), nil
}

func validateUIPort(port string) (string, error) {
	if !uiPortPattern.MatchString(port) {
		return "", fmt.Errorf("invalid light port %q", port)
	}
	return strings.ToUpper(port), nil
}

func requiredUIValue(value *int, label string) (int, error) {
	if value == nil {
		return 0, fmt.Errorf("%s value is required", label)
	}
	return *value, nil
}

func isUITemperature(value int) bool {
	_, ok := uiTemperatureIndex(value)
	return ok
}

func uiTemperatureIndex(value int) (int, bool) {
	for index, step := range uiTemperatureSteps {
		if step == value {
			return index, true
		}
	}
	return 0, false
}

func clampUI(value, min, max int) int {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func intPointer(value int) *int {
	return &value
}

func decodeUIJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	if err := uiJSONContentType(r); err != nil {
		return err
	}
	r.Body = http.MaxBytesReader(w, r.Body, uiBodyLimit)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("request body must contain one JSON object")
		}
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return nil
}

func uiJSONContentType(r *http.Request) error {
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		return errors.New("content type must be application/json")
	}
	return nil
}

// decodeUISettingsJSON is the /api/settings arm of decodeUIJSON (R11-F3):
// the RAW bounded body must be well-formed UTF-8 BEFORE the decoder runs.
// encoding/json silently coerces invalid bytes inside JSON strings to
// U+FFFD on decode, so the daemon-side utf8.ValidString name check would
// otherwise only ever see replacement characters - a direct POST with a
// raw "a\x80b" name would save, apply, or delete the DISTINCT existing
// name "a�b". The whole body is bounded first (the same MaxBytesReader),
// then utf8.Valid gates it, and only then does the decoder run, with the
// same strictness as decodeUIJSON (unknown fields disallowed, one object
// only). The refusal is the fixed string "invalid request encoding".
func decodeUISettingsJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	if err := uiJSONContentType(r); err != nil {
		return err
	}
	r.Body = http.MaxBytesReader(w, r.Body, uiBodyLimit)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	if !utf8.Valid(raw) {
		return errors.New("invalid request encoding")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("request body must contain one JSON object")
		}
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return nil
}

func writeUIJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeUIMethodError(w http.ResponseWriter, method string) {
	w.Header().Set("Allow", method)
	writeUIJSON(w, http.StatusMethodNotAllowed, uiResponse{Error: "method not allowed"})
}

func probeUI(url string) bool {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	response, err := client.Get(url + "api/health")
	if err != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false
	}
	var health struct {
		OK bool `json:"ok"`
	}
	return json.NewDecoder(response.Body).Decode(&health) == nil && health.OK
}

func runUI(args []string, out, errOut io.Writer) int {
	flags := flag.NewFlagSet("ui", flag.ContinueOnError)
	flags.SetOutput(errOut)
	port := flags.Int("port", uiDefaultPort, "loopback HTTP port")
	noOpen := flags.Bool("no-open", false, "do not open the browser")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(errOut, "ui: unexpected arguments:", strings.Join(flags.Args(), " "))
		return 2
	}
	if *port < 1 || *port > 65535 {
		fmt.Fprintln(errOut, "ui: port must be between 1 and 65535")
		return 2
	}

	address := fmt.Sprintf("127.0.0.1:%d", *port)
	url := fmt.Sprintf("http://127.0.0.1:%d/", *port)
	if probeUI(url) {
		if !*noOpen {
			if err := openBrowser(url); err != nil {
				fmt.Fprintln(errOut, "ui: existing panel is running, but browser open failed:", err)
			}
		}
		return 0
	}

	listener, err := net.Listen("tcp", address)
	if err != nil {
		if probeUI(url) {
			if !*noOpen {
				if openErr := openBrowser(url); openErr != nil {
					fmt.Fprintln(errOut, "ui: existing panel is running, but browser open failed:", openErr)
				}
			}
			return 0
		}
		fmt.Fprintf(errOut, "ui: cannot listen on %s: %v\n", address, err)
		return 1
	}

	ui := newUIServer(*port, newDaemonDispatcher())
	ui.snapNow = defaultObsSnapshot
	server := &http.Server{
		Handler:           ui,
		ReadHeaderTimeout: uiReadHeaderTimeout,
		IdleTimeout:       uiIdleTimeout,
		MaxHeaderBytes:    uiMaxHeaderBytes,
	}
	drainDone := make(chan struct{})
	ui.shutdown = func() {
		// http.Server.Shutdown waits for in-flight requests - including the
		// /api/shutdown request itself - so it must run after this handler
		// returns. The small delay lets the reply hit the wire first (the
		// same reply-first pattern as the daemon's UDP shutdown).
		go func() {
			defer close(drainDone)
			time.Sleep(100 * time.Millisecond)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = server.Shutdown(ctx)
		}()
	}
	if !*noOpen {
		go func() {
			deadline := time.Now().Add(2 * time.Second)
			for !probeUI(url) && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if err := openBrowser(url); err != nil {
				fmt.Fprintln(errOut, "ui: panel is running, but browser open failed:", err)
			}
		}()
	}
	if _, err := fmt.Fprintf(out, "mutastic UI listening at %s\n", url); err != nil {
		_ = listener.Close()
		return 1
	}
	serveErr := server.Serve(listener)
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		fmt.Fprintln(errOut, "ui:", serveErr)
		return 1
	}
	// If Serve ended because of the shutdown hook, join the drain before
	// the process exits (the entry point os.Exit()s immediately after this
	// returns). Bounded so a wedged in-flight request cannot hang the exit.
	if errors.Is(serveErr, http.ErrServerClosed) {
		select {
		case <-drainDone:
		case <-time.After(4 * time.Second):
		}
	}
	return 0
}
