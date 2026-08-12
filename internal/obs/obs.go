// Package obs speaks the minimum of the obs-websocket v5 protocol needed
// to grab a still frame from a running OBS Studio: the Hello/Identify
// handshake (with the SHA-256 challenge auth) and the GetSourceScreenshot
// / GetCurrentProgramScene / GetSceneList / GetInputList requests. The
// package is platform-free and transport-free; the real gorilla/websocket
// connection is injected from package main (obs.go) - the same pattern as
// internal/deckplugin's Conn and internal/light's Port.
//
// Protocol shape (obs-websocket v5, rpcVersion 1): every frame is a JSON
// envelope {"op":N,"d":{...}}. The server opens with Hello (op 0), the
// client answers Identify (op 1), the server confirms Identified (op 2);
// after that the client sends Request (op 6) and matches RequestResponse
// (op 7) frames by requestId, skipping any Event (op 5) frames that
// arrive interleaved.
package obs

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Conn is the minimal WebSocket surface the client needs. Implemented by
// package main's gorilla/websocket adapter; tests use a scripted fake.
type Conn interface {
	ReadMessage() ([]byte, error) // blocks until one text frame arrives
	WriteMessage(data []byte) error
}

// ErrAuthRequired means the server's Hello demanded authentication but no
// password was supplied. The CLI translates this into flag/env advice.
var ErrAuthRequired = errors.New("OBS requires authentication but no password was given")

// obs-websocket v5 opcodes (the subset this client speaks).
const (
	opHello           = 0
	opIdentify        = 1
	opIdentified      = 2
	opEvent           = 5
	opRequest         = 6
	opRequestResponse = 7
)

// envelope is the outer frame of every obs-websocket message.
type envelope struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
}

// helloData is the Hello (op 0) payload. Authentication is nil when the
// server has auth disabled - the Identify must then OMIT the auth field.
type helloData struct {
	RPCVersion     int `json:"rpcVersion"`
	Authentication *struct {
		Challenge string `json:"challenge"`
		Salt      string `json:"salt"`
	} `json:"authentication"`
}

// requestResponse is the RequestResponse (op 7) payload.
type requestResponse struct {
	RequestType   string `json:"requestType"`
	RequestID     string `json:"requestId"`
	RequestStatus struct {
		Result  bool   `json:"result"`
		Code    int    `json:"code"`
		Comment string `json:"comment"`
	} `json:"requestStatus"`
	ResponseData json.RawMessage `json:"responseData"`
}

// Client is an identified obs-websocket session. Not safe for concurrent
// use - the CLI is strictly sequential and so is this.
type Client struct {
	conn  Conn
	reqID int
}

// Handshake performs Hello -> Identify -> Identified on a fresh
// connection and returns a ready Client. password may be empty only when
// the server has authentication disabled; if the Hello carries an auth
// challenge and password is empty, ErrAuthRequired is returned before
// anything is sent. eventSubscriptions is pinned to 0: this client wants
// request/response traffic only, not the event firehose.
func Handshake(conn Conn, password string) (*Client, error) {
	data, err := conn.ReadMessage()
	if err != nil {
		return nil, fmt.Errorf("read Hello: %w", err)
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("decode Hello: %w", err)
	}
	if env.Op != opHello {
		return nil, fmt.Errorf("expected Hello (op %d), got op %d", opHello, env.Op)
	}
	var hello helloData
	if err := json.Unmarshal(env.D, &hello); err != nil {
		return nil, fmt.Errorf("decode Hello data: %w", err)
	}

	identify := map[string]any{"rpcVersion": 1, "eventSubscriptions": 0}
	if hello.Authentication != nil {
		if password == "" {
			return nil, ErrAuthRequired
		}
		identify["authentication"] = authResponse(password, hello.Authentication.Salt, hello.Authentication.Challenge)
	}
	if err := writeFrame(conn, opIdentify, identify); err != nil {
		return nil, fmt.Errorf("send Identify: %w", err)
	}

	// A wrong password makes the server close the socket (close code
	// 4009) instead of answering, so a read error here almost always
	// means bad credentials - say so.
	data, err = conn.ReadMessage()
	if err != nil {
		return nil, fmt.Errorf("authentication failed (wrong password?): %w", err)
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("decode Identified: %w", err)
	}
	if env.Op != opIdentified {
		return nil, fmt.Errorf("expected Identified (op %d), got op %d", opIdentified, env.Op)
	}
	return &Client{conn: conn}, nil
}

// authResponse computes the obs-websocket v5 auth string:
// base64(sha256(base64(sha256(password+salt)) + challenge)).
func authResponse(password, salt, challenge string) string {
	secret := sha256.Sum256([]byte(password + salt))
	b64Secret := base64.StdEncoding.EncodeToString(secret[:])
	final := sha256.Sum256([]byte(b64Secret + challenge))
	return base64.StdEncoding.EncodeToString(final[:])
}

// writeFrame marshals one {"op":N,"d":...} envelope and sends it.
func writeFrame(conn Conn, op int, d any) error {
	payload, err := json.Marshal(d)
	if err != nil {
		return err
	}
	frame, err := json.Marshal(envelope{Op: op, D: payload})
	if err != nil {
		return err
	}
	return conn.WriteMessage(frame)
}

// call sends one Request (op 6) and blocks until its RequestResponse
// (op 7) arrives, skipping unrelated frames (Events, or responses to
// other requestIds - impossible today with sequential calls, but cheap
// to be correct about). A failed requestStatus becomes an error carrying
// the server's comment, which is where "no such source" style messages
// live.
func (c *Client) call(requestType string, requestData any) (json.RawMessage, error) {
	c.reqID++
	id := fmt.Sprintf("req-%d", c.reqID)
	req := map[string]any{"requestType": requestType, "requestId": id}
	if requestData != nil {
		req["requestData"] = requestData
	}
	if err := writeFrame(c.conn, opRequest, req); err != nil {
		return nil, fmt.Errorf("%s: send: %w", requestType, err)
	}
	for {
		data, err := c.conn.ReadMessage()
		if err != nil {
			return nil, fmt.Errorf("%s: read reply: %w", requestType, err)
		}
		var env envelope
		if err := json.Unmarshal(data, &env); err != nil {
			return nil, fmt.Errorf("%s: decode reply: %w", requestType, err)
		}
		if env.Op != opRequestResponse {
			continue // Event or other chatter: not for us
		}
		var resp requestResponse
		if err := json.Unmarshal(env.D, &resp); err != nil {
			return nil, fmt.Errorf("%s: decode response data: %w", requestType, err)
		}
		if resp.RequestID != id {
			continue
		}
		if !resp.RequestStatus.Result {
			msg := resp.RequestStatus.Comment
			if msg == "" {
				msg = fmt.Sprintf("request failed with code %d", resp.RequestStatus.Code)
			}
			return nil, fmt.Errorf("%s: %s", requestType, msg)
		}
		return resp.ResponseData, nil
	}
}

// CurrentProgramScene returns the name of the scene OBS is currently
// outputting - the natural default screenshot target (a scene is a valid
// source for GetSourceScreenshot).
func (c *Client) CurrentProgramScene() (string, error) {
	raw, err := c.call("GetCurrentProgramScene", nil)
	if err != nil {
		return "", err
	}
	// currentProgramSceneName is the documented field; sceneName is its
	// 5.4+ duplicate. Accept either.
	var d struct {
		CurrentProgramSceneName string `json:"currentProgramSceneName"`
		SceneName               string `json:"sceneName"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return "", fmt.Errorf("GetCurrentProgramScene: decode: %w", err)
	}
	name := d.CurrentProgramSceneName
	if name == "" {
		name = d.SceneName
	}
	if name == "" {
		return "", errors.New("GetCurrentProgramScene: no scene name in response")
	}
	return name, nil
}

// SnapshotRequest names one GetSourceScreenshot call. Format is the
// obs-websocket imageFormat string ("jpg", "png"); Width/Height are the
// requested render size and omitted from the request when <= 0 (OBS then
// uses the source's native size).
type SnapshotRequest struct {
	Source string
	Format string
	Width  int
	Height int
}

// Snapshot captures one frame of the named source and returns the raw
// image bytes (the base64 data URI in the response, decoded).
func (c *Client) Snapshot(req SnapshotRequest) ([]byte, error) {
	data := map[string]any{"sourceName": req.Source, "imageFormat": req.Format}
	if req.Width > 0 {
		data["imageWidth"] = req.Width
	}
	if req.Height > 0 {
		data["imageHeight"] = req.Height
	}
	raw, err := c.call("GetSourceScreenshot", data)
	if err != nil {
		return nil, err
	}
	var d struct {
		ImageData string `json:"imageData"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("GetSourceScreenshot: decode: %w", err)
	}
	return decodeDataURI(d.ImageData)
}

// decodeDataURI strips a "data:image/...;base64," prefix and decodes the
// remainder.
func decodeDataURI(uri string) ([]byte, error) {
	_, b64, found := strings.Cut(uri, "base64,")
	if !found {
		return nil, fmt.Errorf("imageData is not a base64 data URI (starts %.40q)", uri)
	}
	img, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("decode imageData: %w", err)
	}
	return img, nil
}

// Sources returns the scene names (GetSceneList) and input names
// (GetInputList) - everything GetSourceScreenshot will accept as a
// sourceName.
func (c *Client) Sources() (scenes, inputs []string, err error) {
	raw, err := c.call("GetSceneList", nil)
	if err != nil {
		return nil, nil, err
	}
	var sl struct {
		Scenes []struct {
			SceneName string `json:"sceneName"`
		} `json:"scenes"`
	}
	if err := json.Unmarshal(raw, &sl); err != nil {
		return nil, nil, fmt.Errorf("GetSceneList: decode: %w", err)
	}
	for _, s := range sl.Scenes {
		scenes = append(scenes, s.SceneName)
	}

	raw, err = c.call("GetInputList", nil)
	if err != nil {
		return nil, nil, err
	}
	var il struct {
		Inputs []struct {
			InputName string `json:"inputName"`
		} `json:"inputs"`
	}
	if err := json.Unmarshal(raw, &il); err != nil {
		return nil, nil, fmt.Errorf("GetInputList: decode: %w", err)
	}
	for _, i := range il.Inputs {
		inputs = append(inputs, i.InputName)
	}
	return scenes, inputs, nil
}

// FormatFromPath returns the screenshot format implied by the output
// file's extension: "png" for .png, otherwise "jpg" (the default - small
// files, fine for judging lighting).
func FormatFromPath(path string) string {
	if strings.EqualFold(filepath.Ext(path), ".png") {
		return "png"
	}
	return "jpg"
}
