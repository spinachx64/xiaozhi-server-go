package delegate

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func websocketURL(server *httptest.Server) string {
	return "ws" + strings.TrimPrefix(server.URL, "http")
}

func bridge(t *testing.T, handle func(*websocket.Conn, Frame)) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(Frame{Type: "ready"})
		var task Frame
		if err := conn.ReadJSON(&task); err != nil {
			return
		}
		handle(conn, task)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestNewClientValidates(t *testing.T) {
	for _, tc := range []struct{ url, token string }{
		{"", "t"}, {"http://x", "t"}, {"ws://", "t"}, {"ws://u:p@x", "t"}, {"ws://x", " "},
	} {
		if _, err := NewClient(tc.url, tc.token); err == nil {
			t.Errorf("NewClient(%q, %q) succeeded", tc.url, tc.token)
		}
	}
	if _, err := NewClient("wss://bridge.local/ws", "t"); err != nil {
		t.Fatal(err)
	}
}

func TestRunReturnsResultAndProgress(t *testing.T) {
	server := bridge(t, func(conn *websocket.Conn, task Frame) {
		if task.Type != "task" || task.TaskID != "t1" || task.DeviceID != "aa_bb" || task.Text != "查资料" || task.Context != "ctx" {
			_ = conn.WriteJSON(Frame{Type: "error", TaskID: task.TaskID, Code: "bad_task", Message: "unexpected"})
			return
		}
		_ = conn.WriteJSON(Frame{Type: "accepted", TaskID: "t1"})
		_ = conn.WriteJSON(Frame{Type: "result", TaskID: "other", Text: "wrong"})
		_ = conn.WriteJSON(Frame{Type: "progress", TaskID: "t1", Text: "正在搜索"})
		_ = conn.WriteJSON(Frame{Type: "result", TaskID: "t1", Text: "完成"})
	})
	client, err := NewClient(websocketURL(server), "secret")
	if err != nil {
		t.Fatal(err)
	}
	var progress []string
	got, err := client.Run(context.Background(), Task{ID: "t1", DeviceID: "aa:bb", Text: "查资料", Context: "ctx"}, func(s string) { progress = append(progress, s) })
	if err != nil {
		t.Fatal(err)
	}
	if got != "完成" || len(progress) != 1 || progress[0] != "正在搜索" {
		t.Fatalf("got %q progress %v", got, progress)
	}
}

func TestRunReportsBridgeError(t *testing.T) {
	server := bridge(t, func(conn *websocket.Conn, task Frame) {
		_ = conn.WriteJSON(Frame{Type: "error", TaskID: task.TaskID, Code: "busy", Message: "too many tasks"})
	})
	client, _ := NewClient(websocketURL(server), "secret")
	_, err := client.Run(context.Background(), Task{ID: "t1", DeviceID: "d", Text: "x"}, nil)
	var bridgeErr *BridgeError
	if !errors.As(err, &bridgeErr) || bridgeErr.Code != "busy" {
		t.Fatalf("err = %v", err)
	}
}

func TestRunRejectsBadToken(t *testing.T) {
	server := bridge(t, func(*websocket.Conn, Frame) {})
	client, _ := NewClient(websocketURL(server), "wrong")
	if _, err := client.Run(context.Background(), Task{ID: "t1", DeviceID: "d", Text: "x"}, nil); err == nil {
		t.Fatal("expected auth failure")
	}
}

func TestRunCancelSendsCancelFrame(t *testing.T) {
	cancelled := make(chan Frame, 1)
	server := bridge(t, func(conn *websocket.Conn, task Frame) {
		_ = conn.WriteJSON(Frame{Type: "accepted", TaskID: task.TaskID})
		var frame Frame
		if err := conn.ReadJSON(&frame); err == nil {
			cancelled <- frame
		}
	})
	client, _ := NewClient(websocketURL(server), "secret")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := client.Run(ctx, Task{ID: "t1", DeviceID: "d", Text: "x"}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	select {
	case frame := <-cancelled:
		if frame.Type != "cancel" || frame.TaskID != "t1" {
			t.Fatalf("frame = %+v", frame)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bridge did not receive cancel")
	}
}

func TestBridgeDeviceID(t *testing.T) {
	if got := BridgeDeviceID("aa:bb:cc"); got != "aa_bb_cc" {
		t.Fatalf("got %q", got)
	}
	got := BridgeDeviceID("../etc/passwd")
	if !strings.HasPrefix(got, "h-") || strings.ContainsAny(got, "./") {
		t.Fatalf("got %q", got)
	}
	if BridgeDeviceID("") == "" {
		t.Fatal("empty id must hash")
	}
}

func TestFetchAndAck(t *testing.T) {
	server := bridge(t, func(conn *websocket.Conn, req Frame) {
		switch req.Type {
		case "fetch":
			_ = conn.WriteJSON(Frame{Type: "pending", DeviceID: req.DeviceID, Results: []StoredResult{{TaskID: "t1", Task: "查", Text: "好"}}})
		case "ack":
			if req.DeviceID != "aa_bb" || len(req.TaskIDs) != 1 || req.TaskIDs[0] != "t1" {
				_ = conn.WriteJSON(Frame{Type: "error", Code: "bad_request", Message: "bad ack"})
				return
			}
			_ = conn.WriteJSON(Frame{Type: "acked", Count: 1})
		}
	})
	client, err := NewClient(websocketURL(server), "secret")
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.Fetch(context.Background(), "aa:bb")
	if err != nil || len(got) != 1 || got[0].Text != "好" {
		t.Fatalf("fetch = %+v, %v", got, err)
	}
	if err := client.Ack(context.Background(), "aa:bb", []string{"t1"}); err != nil {
		t.Fatal(err)
	}
}
