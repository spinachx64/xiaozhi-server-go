// Package delegate hands long-running voice tasks to an external agent bridge
// (github.com/spinachx64/agent-bridge) over WebSocket and waits for the answer.
//
// Protocol v1 (JSON text frames, one request per connection):
//
//	bridge -> client  {"type":"ready","protocol":1,"harness":"codex"}
//	task:  client -> {"type":"task","task_id","device_id","text","context","new_session"}
//	       bridge -> {"type":"accepted"}, {"type":"progress","text"}*, then
//	                 {"type":"result","text"} | {"type":"error","code","message"}
//	       client -> {"type":"cancel","task_id"}  (closing the socket does not cancel)
//	fetch: client -> {"type":"fetch","device_id"}            bridge -> {"type":"pending","results":[...]}
//	ack:   client -> {"type":"ack","device_id","task_ids"}   bridge -> {"type":"acked","count"}
//
// The bridge keeps finished tasks until they are acknowledged, so results
// survive a restart of xiaozhi-server.
package delegate

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	maxFrameBytes  = 1 << 20
	requestTimeout = 10 * time.Second
)

// ErrShutdown is the cancel cause used when xiaozhi-server stops. Tasks are
// then left running on the bridge, which stores their results for later.
var ErrShutdown = errors.New("delegate manager shut down")

// Frame is a bridge protocol message.
type Frame struct {
	Type       string         `json:"type"`
	TaskID     string         `json:"task_id,omitempty"`
	DeviceID   string         `json:"device_id,omitempty"`
	Text       string         `json:"text,omitempty"`
	Context    string         `json:"context,omitempty"`
	NewSession bool           `json:"new_session,omitempty"`
	Code       string         `json:"code,omitempty"`
	Message    string         `json:"message,omitempty"`
	Protocol   int            `json:"protocol,omitempty"`
	Harness    string         `json:"harness,omitempty"`
	TaskIDs    []string       `json:"task_ids,omitempty"`
	Results    []StoredResult `json:"results,omitempty"`
	Count      int            `json:"count,omitempty"`
}

// StoredResult is a finished task the bridge keeps until it is acknowledged.
// Exactly one of Text or Code is set.
type StoredResult struct {
	TaskID     string    `json:"task_id"`
	Task       string    `json:"task"`
	Text       string    `json:"text,omitempty"`
	Code       string    `json:"code,omitempty"`
	Message    string    `json:"message,omitempty"`
	FinishedAt time.Time `json:"finished_at"`
}

// Task describes one delegated request.
type Task struct {
	ID         string
	DeviceID   string
	Text       string
	Context    string
	NewSession bool // 不续接设备上一次的 harness 会话
}

// BridgeError is an error reported by the bridge itself.
type BridgeError struct {
	Code    string
	Message string
}

func (e *BridgeError) Error() string {
	if e.Code == "" {
		return "delegate bridge: " + e.Message
	}
	return fmt.Sprintf("delegate bridge %s: %s", e.Code, e.Message)
}

// Client talks to the bridge. It is safe for concurrent use.
type Client struct {
	url    string
	token  string
	dialer *websocket.Dialer
}

// NewClient validates the endpoint and token.
func NewClient(endpoint, token string) (*Client, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil, errors.New("missing delegate bridge URL")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "ws" && parsed.Scheme != "wss") {
		return nil, errors.New("invalid delegate bridge URL: it must use ws:// or wss:// and include a host")
	}
	if parsed.User != nil {
		return nil, errors.New("delegate bridge URL must not contain credentials")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("missing delegate bridge token")
	}
	return &Client{url: parsed.String(), token: token, dialer: &websocket.Dialer{HandshakeTimeout: 10 * time.Second}}, nil
}

// Run submits the task and blocks until the bridge returns a result, reports
// an error, or ctx ends. onProgress, when non-nil, receives progress text.
func (c *Client) Run(ctx context.Context, task Task, onProgress func(string)) (string, error) {
	if strings.TrimSpace(task.Text) == "" {
		return "", errors.New("delegate task text is empty")
	}
	if task.ID == "" {
		return "", errors.New("delegate task id is empty")
	}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+c.token)
	conn, _, err := c.dialer.DialContext(ctx, c.url, headers)
	if err != nil {
		return "", fmt.Errorf("connect delegate bridge: %w", err)
	}
	defer conn.Close()
	conn.SetReadLimit(maxFrameBytes)

	// gorilla/websocket allows one concurrent writer.
	var writeMu sync.Mutex
	write := func(f Frame) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteJSON(f)
	}

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			// Best effort: tell the bridge to abort, then unblock the reader.
			// On shutdown only disconnect; the bridge finishes and stores it.
			if !errors.Is(context.Cause(ctx), ErrShutdown) {
				writeMu.Lock()
				_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
				_ = conn.WriteJSON(Frame{Type: "cancel", TaskID: task.ID})
				writeMu.Unlock()
			}
			_ = conn.Close()
		case <-done:
		}
	}()

	var ready Frame
	if err := conn.ReadJSON(&ready); err != nil {
		return "", c.readError(ctx, "read delegate bridge readiness", err)
	}
	switch ready.Type {
	case "ready":
	case "error":
		return "", &BridgeError{Code: ready.Code, Message: ready.Message}
	default:
		return "", fmt.Errorf("delegate bridge: expected ready frame, got %q", ready.Type)
	}

	if err := write(Frame{
		Type:       "task",
		TaskID:     task.ID,
		DeviceID:   BridgeDeviceID(task.DeviceID),
		Text:       task.Text,
		Context:    task.Context,
		NewSession: task.NewSession,
	}); err != nil {
		return "", c.readError(ctx, "send delegate task", err)
	}

	for {
		var frame Frame
		if err := conn.ReadJSON(&frame); err != nil {
			return "", c.readError(ctx, "read delegate bridge response", err)
		}
		if frame.TaskID != "" && frame.TaskID != task.ID {
			continue
		}
		switch frame.Type {
		case "accepted":
		case "progress":
			if onProgress != nil && frame.Text != "" {
				onProgress(frame.Text)
			}
		case "result":
			return frame.Text, nil
		case "error":
			return "", &BridgeError{Code: frame.Code, Message: frame.Message}
		}
	}
}

// Fetch returns the device's results that the bridge holds but nobody has
// acknowledged yet, oldest first.
func (c *Client) Fetch(ctx context.Context, deviceID string) ([]StoredResult, error) {
	f, err := c.request(ctx, Frame{Type: "fetch", DeviceID: BridgeDeviceID(deviceID)}, "pending")
	if err != nil {
		return nil, err
	}
	return f.Results, nil
}

// Ack tells the bridge the results were delivered, so it drops them.
func (c *Client) Ack(ctx context.Context, deviceID string, taskIDs []string) error {
	if len(taskIDs) == 0 {
		return nil
	}
	_, err := c.request(ctx, Frame{Type: "ack", DeviceID: BridgeDeviceID(deviceID), TaskIDs: taskIDs}, "acked")
	return err
}

// request runs a short request/response exchange on its own connection.
func (c *Client) request(ctx context.Context, req Frame, want string) (Frame, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+c.token)
	conn, _, err := c.dialer.DialContext(ctx, c.url, headers)
	if err != nil {
		return Frame{}, fmt.Errorf("connect delegate bridge: %w", err)
	}
	defer conn.Close()
	conn.SetReadLimit(maxFrameBytes)
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetReadDeadline(deadline)
		_ = conn.SetWriteDeadline(deadline)
	}
	for _, step := range []string{"ready", want} {
		if step == want {
			if err := conn.WriteJSON(req); err != nil {
				return Frame{}, fmt.Errorf("send delegate %s: %w", req.Type, err)
			}
		}
		var f Frame
		if err := conn.ReadJSON(&f); err != nil {
			return Frame{}, c.readError(ctx, "read delegate bridge "+step, err)
		}
		switch f.Type {
		case step:
			if step == want {
				return f, nil
			}
		case "error":
			return Frame{}, &BridgeError{Code: f.Code, Message: f.Message}
		default:
			return Frame{}, fmt.Errorf("delegate bridge: expected %s frame, got %q", step, f.Type)
		}
	}
	return Frame{}, errors.New("unreachable")
}

func (c *Client) readError(ctx context.Context, op string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return fmt.Errorf("%s: %w", op, err)
}

// BridgeDeviceID keeps readable device ids and hashes anything else, so the
// bridge can use it safely as a directory name.
func BridgeDeviceID(id string) string {
	id = strings.ReplaceAll(strings.TrimSpace(id), ":", "_") // MAC 地址
	if id != "" && len(id) <= 128 && strings.IndexFunc(id, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-')
	}) < 0 {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return "h-" + base64.RawURLEncoding.EncodeToString(sum[:18])
}
