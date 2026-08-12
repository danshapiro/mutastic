package obs

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// fakeConn is a scripted obs.Conn: ReadMessage pops pre-queued frames,
// WriteMessage records what the client sent (mirrors the fakes in
// internal/deckplugin and internal/light).
type fakeConn struct {
	reads  [][]byte
	writes [][]byte
}

func (f *fakeConn) ReadMessage() ([]byte, error) {
	if len(f.reads) == 0 {
		return nil, errors.New("fake: connection closed")
	}
	msg := f.reads[0]
	f.reads = f.reads[1:]
	return msg, nil
}

func (f *fakeConn) WriteMessage(data []byte) error {
	f.writes = append(f.writes, data)
	return nil
}

func frame(t *testing.T, op int, d any) []byte {
	t.Helper()
	payload, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(map[string]any{"op": op, "d": json.RawMessage(payload)})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func helloFrame(t *testing.T, withAuth bool) []byte {
	t.Helper()
	d := map[string]any{"obsWebSocketVersion": "5.5.2", "rpcVersion": 1}
	if withAuth {
		d["authentication"] = map[string]string{"challenge": "challengechallenge", "salt": "saltsalt"}
	}
	return frame(t, opHello, d)
}

func identifiedFrame(t *testing.T) []byte {
	t.Helper()
	return frame(t, opIdentified, map[string]any{"negotiatedRpcVersion": 1})
}

func responseFrame(t *testing.T, id string, ok bool, comment string, data any) []byte {
	t.Helper()
	d := map[string]any{
		"requestType":   "x",
		"requestId":     id,
		"requestStatus": map[string]any{"result": ok, "code": 100, "comment": comment},
	}
	if data != nil {
		d["responseData"] = data
	}
	return frame(t, opRequestResponse, d)
}

// decodeWrite unmarshals one recorded client frame.
func decodeWrite(t *testing.T, raw []byte) (op int, d map[string]any) {
	t.Helper()
	var env struct {
		Op int             `json:"op"`
		D  json.RawMessage `json:"d"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(env.D, &d); err != nil {
		t.Fatal(err)
	}
	return env.Op, d
}

func TestHandshakeNoAuth(t *testing.T) {
	conn := &fakeConn{reads: [][]byte{helloFrame(t, false), identifiedFrame(t)}}
	if _, err := Handshake(conn, ""); err != nil {
		t.Fatal(err)
	}
	if len(conn.writes) != 1 {
		t.Fatalf("wrote %d frames, want 1", len(conn.writes))
	}
	op, d := decodeWrite(t, conn.writes[0])
	if op != opIdentify {
		t.Fatalf("op = %d, want Identify (%d)", op, opIdentify)
	}
	if d["rpcVersion"] != float64(1) {
		t.Fatalf("rpcVersion = %v, want 1", d["rpcVersion"])
	}
	if _, present := d["authentication"]; present {
		t.Fatal("Identify carries authentication despite auth-free Hello")
	}
}

func TestHandshakeAuth(t *testing.T) {
	conn := &fakeConn{reads: [][]byte{helloFrame(t, true), identifiedFrame(t)}}
	if _, err := Handshake(conn, "supersecret"); err != nil {
		t.Fatal(err)
	}
	_, d := decodeWrite(t, conn.writes[0])
	// Known vector: base64(sha256(base64(sha256("supersecret"+"saltsalt"))
	// + "challengechallenge")), computed independently of authResponse.
	const want = "HlCzNCzsj6Jqxbg0VV4ylMKNVjcx3H2WG3hEPtaw0Cg="
	if got := d["authentication"]; got != want {
		t.Fatalf("authentication = %v, want %s", got, want)
	}
}

func TestHandshakeAuthRequiredNoPassword(t *testing.T) {
	conn := &fakeConn{reads: [][]byte{helloFrame(t, true)}}
	_, err := Handshake(conn, "")
	if !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("err = %v, want ErrAuthRequired", err)
	}
	if len(conn.writes) != 0 {
		t.Fatal("client sent frames despite refusing to authenticate")
	}
}

func TestHandshakeWrongPasswordReadsAsAuthFailure(t *testing.T) {
	// A wrong password makes the real server close the socket instead of
	// sending Identified; the fake's exhausted-reads error stands in.
	conn := &fakeConn{reads: [][]byte{helloFrame(t, true)}}
	_, err := Handshake(conn, "wrong")
	if err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("err = %v, want authentication failed", err)
	}
}

func TestSnapshot(t *testing.T) {
	img := []byte{0xFF, 0xD8, 0xFF, 0xE0, 1, 2, 3} // JPEG-ish bytes
	uri := "data:image/jpg;base64," + base64.StdEncoding.EncodeToString(img)
	conn := &fakeConn{reads: [][]byte{
		helloFrame(t, false),
		identifiedFrame(t),
		frame(t, opEvent, map[string]any{"eventType": "SceneNameChanged"}),          // interleaved chatter: skipped
		responseFrame(t, "req-999", true, "", map[string]any{"imageData": "wrong"}), // foreign id: skipped
		responseFrame(t, "req-1", true, "", map[string]any{"imageData": uri}),
	}}
	c, err := Handshake(conn, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Snapshot(SnapshotRequest{Source: "Webcam", Format: "jpg", Width: 1280, Height: 720})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(img) {
		t.Fatalf("image bytes = %v, want %v", got, img)
	}

	op, d := decodeWrite(t, conn.writes[1]) // writes[0] is Identify
	if op != opRequest {
		t.Fatalf("op = %d, want Request (%d)", op, opRequest)
	}
	if d["requestType"] != "GetSourceScreenshot" {
		t.Fatalf("requestType = %v", d["requestType"])
	}
	rd := d["requestData"].(map[string]any)
	if rd["sourceName"] != "Webcam" || rd["imageFormat"] != "jpg" ||
		rd["imageWidth"] != float64(1280) || rd["imageHeight"] != float64(720) {
		t.Fatalf("requestData = %v", rd)
	}
}

func TestSnapshotOmitsNonPositiveSize(t *testing.T) {
	uri := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte{1})
	conn := &fakeConn{reads: [][]byte{
		helloFrame(t, false), identifiedFrame(t),
		responseFrame(t, "req-1", true, "", map[string]any{"imageData": uri}),
	}}
	c, err := Handshake(conn, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Snapshot(SnapshotRequest{Source: "s", Format: "png"}); err != nil {
		t.Fatal(err)
	}
	_, d := decodeWrite(t, conn.writes[1])
	rd := d["requestData"].(map[string]any)
	if _, present := rd["imageWidth"]; present {
		t.Fatal("imageWidth sent despite zero Width")
	}
	if _, present := rd["imageHeight"]; present {
		t.Fatal("imageHeight sent despite zero Height")
	}
}

func TestCallFailureSurfacesComment(t *testing.T) {
	conn := &fakeConn{reads: [][]byte{
		helloFrame(t, false), identifiedFrame(t),
		responseFrame(t, "req-1", false, "No source was found by the name of `nope`.", nil),
	}}
	c, err := Handshake(conn, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Snapshot(SnapshotRequest{Source: "nope", Format: "jpg"})
	if err == nil || !strings.Contains(err.Error(), "No source was found") {
		t.Fatalf("err = %v, want server comment surfaced", err)
	}
}

func TestCurrentProgramScene(t *testing.T) {
	for name, data := range map[string]map[string]any{
		"documented field": {"currentProgramSceneName": "Webcam Scene"},
		"5.4+ duplicate":   {"sceneName": "Webcam Scene"},
	} {
		t.Run(name, func(t *testing.T) {
			conn := &fakeConn{reads: [][]byte{
				helloFrame(t, false), identifiedFrame(t),
				responseFrame(t, "req-1", true, "", data),
			}}
			c, err := Handshake(conn, "")
			if err != nil {
				t.Fatal(err)
			}
			scene, err := c.CurrentProgramScene()
			if err != nil {
				t.Fatal(err)
			}
			if scene != "Webcam Scene" {
				t.Fatalf("scene = %q", scene)
			}
		})
	}
}

func TestSources(t *testing.T) {
	conn := &fakeConn{reads: [][]byte{
		helloFrame(t, false), identifiedFrame(t),
		responseFrame(t, "req-1", true, "", map[string]any{
			"scenes": []map[string]any{{"sceneName": "Scene A"}, {"sceneName": "Scene B"}},
		}),
		responseFrame(t, "req-2", true, "", map[string]any{
			"inputs": []map[string]any{{"inputName": "Webcam"}},
		}),
	}}
	c, err := Handshake(conn, "")
	if err != nil {
		t.Fatal(err)
	}
	scenes, inputs, err := c.Sources()
	if err != nil {
		t.Fatal(err)
	}
	if len(scenes) != 2 || scenes[0] != "Scene A" || scenes[1] != "Scene B" {
		t.Fatalf("scenes = %v", scenes)
	}
	if len(inputs) != 1 || inputs[0] != "Webcam" {
		t.Fatalf("inputs = %v", inputs)
	}
}

func TestDecodeDataURIRejectsNonDataURI(t *testing.T) {
	if _, err := decodeDataURI("not an image"); err == nil {
		t.Fatal("want error for non-data-URI imageData")
	}
	if _, err := decodeDataURI("data:image/jpg;base64,@@not-base64@@"); err == nil {
		t.Fatal("want error for invalid base64")
	}
}

func TestFormatFromPath(t *testing.T) {
	for path, want := range map[string]string{
		"shot.png":     "png",
		"SHOT.PNG":     "png",
		"shot.jpg":     "jpg",
		"shot.jpeg":    "jpg",
		"no-extension": "jpg",
	} {
		if got := FormatFromPath(path); got != want {
			t.Errorf("FormatFromPath(%q) = %q, want %q", path, got, want)
		}
	}
}
