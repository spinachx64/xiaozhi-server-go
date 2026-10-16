package dashscope

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"xiaozhi-server-go/src/configs"
	"xiaozhi-server-go/src/core/provider"
	"xiaozhi-server-go/src/core/providers/tts"
	"xiaozhi-server-go/src/core/utils"

	"github.com/gorilla/websocket"
)

func mockTTS(t *testing.T, handler func(*websocket.Conn), deleteFile bool) *Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" || r.URL.Query().Get("model") != "qwen3-tts-flash-realtime" || len(r.URL.Query()["model"]) != 1 {
			t.Error("incorrect authentication or model URL")
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
		handler(conn)
	}))
	t.Cleanup(srv.Close)
	cfg := configs.DefaultDashScopeTTS()
	cfg.APIKey, cfg.URL, cfg.OutputDir = "test-key", "ws"+strings.TrimPrefix(srv.URL, "http")+"?model=overwritten", t.TempDir()
	// Exercise the production factory mapping, not just the direct constructor.
	factory := provider.NewTTSFactory("AliyunTTS", &configs.Config{TTS: map[string]configs.TTSConfig{"AliyunTTS": cfg}, DeleteAudio: deleteFile}, nil)
	created, err := factory.Create()
	if err != nil {
		t.Fatal(err)
	}
	p := created.(*Provider)
	t.Cleanup(func() { p.Cleanup() })
	return p
}

func receiveText(t *testing.T, conn *websocket.Conn, voice string) string {
	t.Helper()
	if err := conn.WriteJSON(map[string]any{"type": "session.created"}); err != nil {
		t.Error(err)
		return ""
	}
	var update struct {
		Type, EventID string
		Session       map[string]any
	}
	// event_id contains an underscore, so decode it explicitly below.
	var raw map[string]any
	if err := conn.ReadJSON(&raw); err != nil {
		t.Error(err)
		return ""
	}
	update.Type, _ = raw["type"].(string)
	update.Session, _ = raw["session"].(map[string]any)
	if update.Type != "session.update" || update.Session["voice"] != voice || update.Session["mode"] != "server_commit" || update.Session["response_format"] != "pcm" || update.Session["sample_rate"] != float64(24000) {
		t.Errorf("invalid session update: %v", raw)
	}
	ids := map[string]bool{}
	var text strings.Builder
	for {
		id, _ := raw["event_id"].(string)
		if !strings.HasPrefix(id, "event_") || ids[id] {
			t.Errorf("invalid event ID %q", id)
		}
		ids[id] = true
		if raw["type"] == "session.update" {
			if err := conn.WriteJSON(map[string]any{"type": "session.updated"}); err != nil {
				t.Error(err)
				return ""
			}
		} else if raw["type"] == "input_text_buffer.append" {
			chunk, _ := raw["text"].(string)
			if len([]rune(chunk)) > 50 {
				t.Error("text chunk too long")
			}
			text.WriteString(chunk)
		} else if raw["type"] == "session.finish" {
			return text.String()
		} else {
			t.Errorf("unexpected client event: %v", raw)
			return ""
		}
		raw = nil
		if err := conn.ReadJSON(&raw); err != nil {
			t.Error(err)
			return ""
		}
	}
}

func TestSynthesisVoiceSelectionAndPlaybackWAV(t *testing.T) {
	pcm := bytes.Repeat([]byte{1, 0}, 2400)
	text := strings.Repeat("你好", 60) + "，Hello world."
	p := mockTTS(t, func(conn *websocket.Conn) {
		if got := receiveText(t, conn, "Serena"); got != text {
			t.Errorf("text changed: %q", got)
		}
		for _, part := range [][]byte{pcm[:1], pcm[1:]} {
			if err := conn.WriteJSON(map[string]any{"type": "response.audio.delta", "delta": base64.StdEncoding.EncodeToString(part)}); err != nil {
				t.Error(err)
				return
			}
		}
		conn.WriteJSON(map[string]any{"type": "response.done", "response": map[string]any{"status": "completed"}})
		conn.WriteJSON(map[string]any{"type": "session.finished"})
	}, true)
	if err, name := p.SetVoice("苏瑶"); err != nil || name != "Serena" {
		t.Fatalf("%q %v", name, err)
	}
	if err, _ := p.SetVoice("Raymond"); err == nil {
		t.Fatal("unsupported voice accepted")
	}
	path, err := p.ToTTS(text)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data[:4]) != "RIFF" || binary.LittleEndian.Uint32(data[24:28]) != 24000 || binary.LittleEndian.Uint32(data[40:44]) != uint32(len(pcm)) {
		t.Fatal("invalid WAV header")
	}
	decoded, err := utils.ReadPCMDataFromWavFile(path)
	if err != nil || !bytes.Equal(decoded, pcm) {
		t.Fatalf("playback decoder: %v", err)
	}
	snapshot := p.Config()
	snapshot.Voice = "Ethan"
	snapshot.SupportedVoices[0].Name = "changed"
	if p.Config().Voice != "Serena" || p.Config().SupportedVoices[0].Name != "Cherry" {
		t.Fatal("configuration snapshot was mutable")
	}
	if err := p.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("generated audio was not cleaned up")
	}
}

func TestServiceFailuresNeverReturnPartialAudio(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []map[string]any
		want   string
	}{
		{"voice error", []map[string]any{{"type": "error", "error": map[string]any{"code": "CLIENT_ERROR", "message": "Invalid voice test-key"}}}, "CLIENT_ERROR"},
		{"bad base64", []map[string]any{{"type": "response.audio.delta", "delta": "%%%"}}, "base64"},
		{"empty audio", []map[string]any{{"type": "session.finished"}}, "empty"},
		{"odd PCM", []map[string]any{{"type": "response.audio.delta", "delta": "AQ=="}, {"type": "session.finished"}}, "incomplete"},
		{"failed response", []map[string]any{{"type": "response.done", "response": map[string]any{"status": "failed"}}}, "failed"},
		{"premature close", []map[string]any{{"type": "response.audio.delta", "delta": "AQI="}}, "session.finished"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := mockTTS(t, func(conn *websocket.Conn) {
				receiveText(t, conn, "Cherry")
				for _, event := range tc.events {
					if err := conn.WriteJSON(event); err != nil {
						t.Error(err)
						return
					}
				}
			}, false)
			path, err := p.ToTTS("测试")
			if path != "" || err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "test-key") {
				t.Fatalf("path=%q error=%v", path, err)
			}
			files, _ := os.ReadDir(p.Config().OutputDir)
			if len(files) != 0 {
				t.Fatal("failed synthesis left audio files")
			}
		})
	}
}

func TestCancellationAndCleanupCloseSocket(t *testing.T) {
	for _, cleanup := range []bool{false, true} {
		t.Run(map[bool]string{false: "context", true: "cleanup"}[cleanup], func(t *testing.T) {
			started := make(chan struct{})
			p := mockTTS(t, func(conn *websocket.Conn) { receiveText(t, conn, "Cherry"); close(started); conn.ReadMessage() }, false)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { _, err := p.ToTTSWithContext(ctx, "测试"); result <- err }()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("session did not start")
			}
			if cleanup {
				p.Cleanup()
			} else {
				cancel()
			}
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("synthesis did not cancel")
			}
		})
	}
}

func TestDefaultsAndModelSpecificVoices(t *testing.T) {
	t.Setenv("DASHSCOPE_API_KEY", "environment-key")
	created, err := NewProvider(&tts.Config{OutputDir: t.TempDir()}, false)
	if err != nil {
		t.Fatal(err)
	}
	p := created.(*Provider)
	if p.Config().APIKey != "environment-key" || len(p.Config().SupportedVoices) != 9 || p.Config().Voice != "Cherry" {
		t.Fatal(p.Config().Voice)
	}
	for _, cfg := range []*tts.Config{
		{Voice: "cherry"}, {Voice: "Raymond"}, {SampleRate: 16000}, {Format: "mp3"}, {TimeoutSeconds: -1}, {Model: "another-model"},
		{SupportedVoices: []configs.VoiceInfo{{Name: "Cherry"}, {Name: "Cherry"}}},
	} {
		if _, err := NewProvider(cfg, false); err == nil {
			t.Errorf("accepted invalid config: %+v", cfg)
		}
	}
	custom, err := NewProvider(&tts.Config{Model: "custom-model", Voice: "CustomVoice", SupportedVoices: []configs.VoiceInfo{{Name: "CustomVoice", DisplayName: "自定义音色"}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err, name := custom.SetVoice("自定义音色"); err != nil || name != "CustomVoice" {
		t.Fatalf("%q %v", name, err)
	}
	t.Setenv("DASHSCOPE_API_KEY", "")
	if _, err := NewProvider(&tts.Config{}, false); err == nil {
		t.Fatal("missing credentials accepted")
	}
}
