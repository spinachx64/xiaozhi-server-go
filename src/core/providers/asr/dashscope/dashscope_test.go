package dashscope

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xiaozhi-server-go/src/core/providers/asr"

	"github.com/gorilla/websocket"
)

func testProvider(t *testing.T, handler func(*websocket.Conn, map[string]any), config map[string]any) *Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "bearer test-key" {
			t.Error("missing bearer authentication")
			w.WriteHeader(401)
			return
		}
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var run map[string]any
		if err := conn.ReadJSON(&run); err != nil {
			t.Error(err)
			return
		}
		header, _ := run["header"].(map[string]any)
		if header["action"] != "run-task" || header["streaming"] != "duplex" || len(header["task_id"].(string)) != 32 {
			t.Errorf("invalid run-task: %v", run)
			return
		}
		handler(conn, run)
	}))
	t.Cleanup(srv.Close)
	data := map[string]any{"api_key": "test-key", "asr_url": "ws" + strings.TrimPrefix(srv.URL, "http"), "timeout_seconds": 2}
	for key, value := range config {
		data[key] = value
	}
	p, err := NewProvider(&asr.Config{Data: data}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.CloseConnection() })
	return p
}

func sendEvent(t *testing.T, conn *websocket.Conn, run map[string]any, name string, sentence map[string]any) {
	t.Helper()
	id := run["header"].(map[string]any)["task_id"]
	msg := map[string]any{"header": map[string]any{"event": name, "task_id": id}, "payload": map[string]any{"output": map[string]any{"sentence": sentence}}}
	if err := conn.WriteJSON(msg); err != nil {
		t.Error(err)
	}
}

func readAudioAndFinish(t *testing.T, conn *websocket.Conn, run map[string]any) {
	t.Helper()
	kind, data, err := conn.ReadMessage()
	if err != nil {
		t.Error(err)
		return
	}
	if kind != websocket.BinaryMessage || string(data) != "\x01\x02\x03\x04" {
		t.Errorf("wrong audio: %d %v", kind, data)
	}
	var finish map[string]any
	if err := conn.ReadJSON(&finish); err != nil {
		t.Error(err)
		return
	}
	header := finish["header"].(map[string]any)
	if header["action"] != "finish-task" || header["task_id"] != run["header"].(map[string]any)["task_id"] {
		t.Errorf("invalid finish-task: %v", finish)
	}
}

func TestTranscribeProtocolAndSentenceRevisions(t *testing.T) {
	p := testProvider(t, func(conn *websocket.Conn, run map[string]any) {
		payload := run["payload"].(map[string]any)
		params := payload["parameters"].(map[string]any)
		if payload["model"] != "paraformer-realtime-v2" || payload["task"] != "asr" || payload["function"] != "recognition" || params["format"] != "pcm" || params["sample_rate"] != float64(16000) {
			t.Errorf("unexpected parameters: %v", payload)
		}
		if hints, _ := params["language_hints"].([]any); len(hints) != 2 {
			t.Errorf("missing hints: %v", params)
		}
		if params["semantic_punctuation_enabled"] != false || params["max_sentence_silence"] != float64(300) {
			t.Errorf("low-latency VAD defaults missing: %v", params)
		}
		sendEvent(t, conn, run, "task-started", nil)
		readAudioAndFinish(t, conn, run)
		for _, sentence := range []map[string]any{
			{"sentence_id": 1, "text": "临时", "sentence_end": false},
			{"sentence_id": 1, "text": "你好。", "sentence_end": true},
			{"sentence_id": 1, "text": "你好！", "sentence_end": true},
			{"sentence_id": 2, "text": "ignored", "sentence_end": true, "heartbeat": true},
			{"sentence_id": 2, "text": "Hello world.", "sentence_end": true},
		} {
			sendEvent(t, conn, run, "result-generated", sentence)
		}
		sendEvent(t, conn, run, "task-finished", nil)
	}, map[string]any{"language_hints": []any{"zh", "en"}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	text, err := p.Transcribe(ctx, []byte{1, 2, 3, 4})
	if err != nil || text != "你好！Hello world." {
		t.Fatalf("%q %v", text, err)
	}
}

type listener struct{ results chan string }

func (l *listener) OnAsrResult(text string, final bool) bool {
	if final {
		l.results <- text
	}
	return true
}

func TestManualStreamingAggregatesOnlyFinalSentences(t *testing.T) {
	p := testProvider(t, func(conn *websocket.Conn, run map[string]any) {
		params := run["payload"].(map[string]any)["parameters"].(map[string]any)
		if params["sample_rate"] != float64(24000) {
			t.Errorf("device sample rate lost: %v", params)
		}
		sendEvent(t, conn, run, "task-started", nil)
		readAudioAndFinish(t, conn, run)
		sendEvent(t, conn, run, "result-generated", map[string]any{"begin_time": 0, "text": "未完成", "sentence_end": false})
		sendEvent(t, conn, run, "result-generated", map[string]any{"begin_time": 0, "text": "第一句。", "sentence_end": true})
		sendEvent(t, conn, run, "result-generated", map[string]any{"begin_time": 1000, "text": "第二句。", "sentence_end": true})
		sendEvent(t, conn, run, "task-finished", nil)
	}, nil)
	l := &listener{results: make(chan string, 2)}
	p.SetListener(l)
	if err := p.ConfigureInput(24000, 1, "manual"); err != nil {
		t.Fatal(err)
	}
	if err := p.AddAudio([]byte{1, 2, 3, 4}); err != nil {
		t.Fatal(err)
	}
	if err := p.SendLastAudio(nil); err != nil {
		t.Fatal(err)
	}
	select {
	case text := <-l.results:
		if text != "第一句。第二句。" {
			t.Fatal(text)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("missing final callback")
	}
	select {
	case text := <-l.results:
		t.Fatalf("duplicate callback %q", text)
	default:
	}
}

func TestAutomaticStreamingEndsAtSentenceBoundary(t *testing.T) {
	p := testProvider(t, func(conn *websocket.Conn, run map[string]any) {
		sendEvent(t, conn, run, "task-started", nil)
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Error(err)
			return
		}
		sendEvent(t, conn, run, "result-generated", map[string]any{"sentence_id": 1, "text": "你好。", "sentence_end": true})
		var finish map[string]any
		if err := conn.ReadJSON(&finish); err != nil {
			t.Error(err)
			return
		}
		if finish["header"].(map[string]any)["action"] != "finish-task" {
			t.Error(finish)
		}
	}, nil)
	l := &listener{results: make(chan string, 1)}
	p.SetListener(l)
	if err := p.AddAudio([]byte{1, 2}); err != nil {
		t.Fatal(err)
	}
	select {
	case text := <-l.results:
		if text != "你好。" {
			t.Fatal(text)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("automatic turn did not end")
	}
}

func TestTaskFailureAndCancellation(t *testing.T) {
	t.Run("service failure", func(t *testing.T) {
		p := testProvider(t, func(conn *websocket.Conn, run map[string]any) {
			conn.WriteJSON(map[string]any{"header": map[string]any{"event": "task-failed", "task_id": run["header"].(map[string]any)["task_id"], "error_code": "ModelNotFound", "error_message": "model not found test-key"}})
		}, nil)
		_, err := p.Transcribe(context.Background(), []byte{1, 2})
		if err == nil || !strings.Contains(err.Error(), "ModelNotFound") || strings.Contains(err.Error(), "test-key") {
			t.Fatal(err)
		}
	})
	t.Run("cancel waiting for task-started", func(t *testing.T) {
		p := testProvider(t, func(conn *websocket.Conn, _ map[string]any) { conn.ReadMessage() }, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err := p.Transcribe(ctx, []byte{1, 2})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	})
	t.Run("cancel waiting for results", func(t *testing.T) {
		p := testProvider(t, func(conn *websocket.Conn, run map[string]any) {
			sendEvent(t, conn, run, "task-started", nil)
			readAudioAndFinish(t, conn, run)
			conn.ReadMessage()
		}, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, err := p.Transcribe(ctx, []byte{1, 2, 3, 4})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	})
}

func TestConfigurationAndFactory(t *testing.T) {
	t.Setenv("DASHSCOPE_API_KEY", "")
	for _, data := range []map[string]any{
		{}, {"api_key": "key", "format": "wav"}, {"api_key": "key", "asr_url": "https://invalid"},
		{"api_key": "key", "sample_rate": -1}, {"api_key": "key", "timeout_seconds": 0},
		{"api_key": "key", "model": "paraformer-realtime-v1", "language_hints": []string{"zh"}},
		{"api_key": "key", "language_hints": "zh"}, {"api_key": "key", "sample_rate": 16000.5},
	} {
		if _, err := NewProvider(&asr.Config{Data: data}, false, nil); err == nil {
			t.Errorf("accepted invalid config %v", data)
		}
	}
	t.Setenv("DASHSCOPE_API_KEY", "environment-key")
	created, err := asr.Create("dashscope", &asr.Config{Data: map[string]any{}}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := created.(*Provider)
	if p.apiKey != "environment-key" {
		t.Fatal("environment key not loaded")
	}
	if err := p.ConfigureInput(16000, 2, "auto"); err == nil {
		t.Fatal("stereo accepted")
	}
	if err := p.AddAudio([]byte{1}); err == nil {
		t.Fatal("incomplete PCM sample accepted")
	}
	if err := p.SendLastAudio(nil); err != nil {
		t.Fatal(err)
	}
}

func TestResetDoesNotLetOldReaderCloseNewTask(t *testing.T) {
	var connections atomic.Int32
	p := testProvider(t, func(conn *websocket.Conn, run map[string]any) {
		number := connections.Add(1)
		sendEvent(t, conn, run, "task-started", nil)
		if number == 1 {
			// The first task is canceled while its reader is blocked on the socket.
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}
		readAudioAndFinish(t, conn, run)
		sendEvent(t, conn, run, "result-generated", map[string]any{"sentence_id": 1, "text": "新任务。", "sentence_end": true})
		sendEvent(t, conn, run, "task-finished", nil)
	}, nil)
	if err := p.AddAudio([]byte{1, 2}); err != nil {
		t.Fatal(err)
	}
	if err := p.Reset(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	text, err := p.Transcribe(ctx, []byte{1, 2, 3, 4})
	if err != nil || text != "新任务。" {
		t.Fatalf("%q %v", text, err)
	}
}

func TestMalformedEventAndUnexpectedDisconnect(t *testing.T) {
	for _, invalid := range []string{"not-json", ""} {
		t.Run("event="+invalid, func(t *testing.T) {
			p := testProvider(t, func(conn *websocket.Conn, _ map[string]any) {
				if invalid != "" {
					conn.WriteMessage(websocket.TextMessage, []byte(invalid))
				}
			}, nil)
			_, err := p.Transcribe(context.Background(), []byte{1, 2})
			if err == nil {
				t.Fatal("invalid service response accepted")
			}
		})
	}
}

type speechListener struct {
	speech chan struct{}
	final  chan string
}

func (l *speechListener) OnAsrSpeechStart() { l.speech <- struct{}{} }
func (l *speechListener) OnAsrResult(text string, final bool) bool {
	if final {
		l.final <- text
	}
	return true
}

func TestInterimSpeechSignalDoesNotCommitPartialText(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	finish := func() { releaseOnce.Do(func() { close(release) }) }
	defer finish()
	p := testProvider(t, func(conn *websocket.Conn, run map[string]any) {
		sendEvent(t, conn, run, "task-started", nil)
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Error(err)
			return
		}
		sendEvent(t, conn, run, "result-generated", map[string]any{"sentence_id": 1, "text": "请等", "sentence_end": false})
		<-release
		sendEvent(t, conn, run, "result-generated", map[string]any{"sentence_id": 1, "text": "请等一下。", "sentence_end": true})
		conn.ReadMessage()
	}, nil)
	l := &speechListener{speech: make(chan struct{}, 2), final: make(chan string, 2)}
	p.SetListener(l)
	if err := p.AddAudio([]byte{1, 2}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-l.speech:
	case <-time.After(time.Second):
		t.Fatal("no early speech signal")
	}
	select {
	case text := <-l.final:
		t.Fatalf("partial transcript committed: %q", text)
	default:
	}
	finish()
	select {
	case text := <-l.final:
		if text != "请等一下。" {
			t.Fatal(text)
		}
	case <-time.After(time.Second):
		t.Fatal("no final result")
	}
	p.mu.Lock()
	old := p.current
	p.mu.Unlock()
	for i := 0; i < 5; i++ {
		if err := p.AddAudio([]byte{0, 0}); err != nil {
			t.Fatal(err)
		}
	}
	p.mu.Lock()
	current := p.current
	p.mu.Unlock()
	if current != old {
		t.Fatal("completed utterance tail created a new cloud task")
	}
}

func TestResetCancelsPendingStartup(t *testing.T) {
	seen := make(chan struct{})
	p := testProvider(t, func(conn *websocket.Conn, _ map[string]any) { close(seen); conn.ReadMessage() }, nil)
	started := make(chan error, 1)
	go func() { started <- p.StartStreaming(context.Background()) }()
	select {
	case <-seen:
	case <-time.After(time.Second):
		t.Fatal("startup did not begin")
	}
	reset := make(chan struct{})
	go func() { p.Reset(); close(reset) }()
	select {
	case <-reset:
	case <-time.After(time.Second):
		t.Fatal("reset waited for startup timeout")
	}
	select {
	case err := <-started:
		if err == nil {
			t.Fatal("canceled startup succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("startup did not cancel")
	}
}
