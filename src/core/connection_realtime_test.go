package core

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xiaozhi-server-go/src/configs"
	"xiaozhi-server-go/src/core/providers/asr"
	"xiaozhi-server-go/src/core/providers/tts"
	dashscopetts "xiaozhi-server-go/src/core/providers/tts/dashscope"
	"xiaozhi-server-go/src/core/utils"

	"github.com/gorilla/websocket"
)

type speechPacket struct {
	kind int
	data []byte
}
type speechTestConn struct {
	Connection
	packets chan speechPacket
}

func (c *speechTestConn) WriteMessage(kind int, data []byte) error {
	c.packets <- speechPacket{kind, append([]byte(nil), data...)}
	return nil
}

type queuedASR struct {
	*asr.BaseProvider
	events chan string
	block  <-chan struct{}
	resets atomic.Int32
}

func (p *queuedASR) Transcribe(context.Context, []byte) (string, error) { return "", nil }
func (p *queuedASR) AddAudio(data []byte) error {
	p.events <- "audio:" + string(data)
	if string(data) == "first" && p.block != nil {
		<-p.block
	}
	return nil
}
func (p *queuedASR) SendLastAudio([]byte) error { p.events <- "end"; return nil }
func (p *queuedASR) Reset() error               { p.resets.Add(1); return nil }
func (p *queuedASR) CloseConnection() error     { return nil }
func (p *queuedASR) ConfigureInput(rate, channels int, mode string) error {
	p.events <- fmt.Sprintf("configure:%d:%d:%s", rate, channels, mode)
	return nil
}
func (p *queuedASR) Prepare(context.Context) error { p.events <- "prepare"; return nil }

func realtimeHandler(t *testing.T, format string) *ConnectionHandler {
	t.Helper()
	logger, err := utils.NewLogger(&utils.LogCfg{LogDir: t.TempDir(), LogFile: "test.log", LogLevel: "error"})
	if err != nil {
		t.Fatal(err)
	}
	h := &ConnectionHandler{config: &configs.Config{DeleteAudio: true}, logger: logger, ctx: context.Background(), stopChan: make(chan struct{}), audioMessagesQueue: make(chan audioMessage, 10), clientAudioQueue: make(chan audioInput, 10), serverAudioFormat: format, serverAudioSampleRate: 24000, serverAudioChannels: 1, serverAudioFrameDuration: 60}
	h.conn = &speechTestConn{packets: make(chan speechPacket, 100)}
	h.talkRound.Store(1)
	h.tts_last_text_index.Store(1)
	h.providers.asr = &queuedASR{BaseProvider: asr.NewBaseProvider(&asr.Config{}, false), events: make(chan string, 10)}
	t.Cleanup(func() { h.cancelSpeech(); close(h.stopChan); logger.Close() })
	return h
}

func nextEvent(t *testing.T, events <-chan string, want string) {
	t.Helper()
	select {
	case got := <-events:
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("missing %q", want)
	}
}

func TestASRStartAudioAndStopUseOneFIFO(t *testing.T) {
	h := realtimeHandler(t, "pcm")
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	p := h.providers.asr.(*queuedASR)
	p.block = release
	done := make(chan struct{})
	go func() { h.processClientAudioMessagesCoroutine(); close(done) }()
	h.enqueueAudio(audioInput{start: &asrInput{rate: 24000, channels: 1, mode: "manual"}})
	h.enqueueAudio(audioInput{data: []byte("first")})
	h.enqueueAudio(audioInput{data: []byte("tail")})
	h.enqueueAudio(audioInput{end: true})
	nextEvent(t, p.events, "configure:24000:1:manual")
	nextEvent(t, p.events, "prepare")
	nextEvent(t, p.events, "audio:first")
	select {
	case event := <-p.events:
		t.Fatalf("audio/end bypassed blocked first frame: %s", event)
	default:
	}
	unblock()
	nextEvent(t, p.events, "audio:tail")
	nextEvent(t, p.events, "end")
}

func TestCloudAudioReachesDeviceBeforeSynthesisFinishes(t *testing.T) {
	for _, format := range []string{"pcm", "opus"} {
		t.Run(format, func(t *testing.T) {
			h := realtimeHandler(t, format)
			release := make(chan struct{})
			var once sync.Once
			finish := func() { once.Do(func() { close(release) }) }
			defer finish()
			pcm := bytes.Repeat([]byte{1, 0}, 1440)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer c.Close()
				c.SetReadDeadline(time.Now().Add(3 * time.Second))
				c.WriteJSON(map[string]any{"type": "session.created"})
				var message map[string]any
				if err := c.ReadJSON(&message); err != nil {
					t.Error(err)
					return
				}
				c.WriteJSON(map[string]any{"type": "session.updated"})
				for {
					message = nil
					if err := c.ReadJSON(&message); err != nil {
						t.Error(err)
						return
					}
					if message["type"] == "session.finish" {
						break
					}
				}
				c.WriteJSON(map[string]any{"type": "response.audio.delta", "delta": base64.StdEncoding.EncodeToString(pcm)})
				<-release
				c.WriteJSON(map[string]any{"type": "session.finished"})
			}))
			defer func() { finish(); srv.Close() }()
			created, err := dashscopetts.NewProvider(&tts.Config{APIKey: "test-key", URL: "ws" + strings.TrimPrefix(srv.URL, "http"), OutputDir: t.TempDir()}, false)
			if err != nil {
				t.Fatal(err)
			}
			defer created.Cleanup()
			h.providers.tts = created
			produced := make(chan struct{})
			go func() { h.processTTSTask("你好", 1, 1); close(produced) }()
			var task audioMessage
			select {
			case task = <-h.audioMessagesQueue:
			case <-time.After(2 * time.Second):
				t.Fatal("stream not queued before cloud completion")
			}
			if task.stream == nil {
				t.Fatal("file path used instead of realtime stream")
			}
			sent := make(chan struct{})
			go func() { h.sendStreamingSpeech(task); close(sent) }()
			packets := h.conn.(*speechTestConn).packets
			var audio []byte
			for audio == nil {
				select {
				case packet := <-packets:
					if packet.kind == 2 {
						audio = packet.data
					} else {
						var event map[string]any
						json.Unmarshal(packet.data, &event)
						if event["audio_codec"] != format {
							t.Fatalf("incorrect codec metadata: %v", event)
						}
					}
				case <-time.After(2 * time.Second):
					t.Fatal("first cloud delta did not reach device")
				}
			}
			select {
			case <-produced:
				t.Fatal("test did not hold synthesis open")
			default:
			}
			if format == "pcm" && !bytes.Equal(audio, pcm) {
				t.Fatal("PCM changed or container header sent to device")
			}
			if format == "opus" {
				decoder, err := utils.NewOpusDecoder(&utils.OpusDecoderConfig{SampleRate: 24000, MaxChannels: 1})
				if err != nil {
					t.Fatal(err)
				}
				decoded, err := decoder.Decode(audio)
				decoder.Close()
				if err != nil || len(decoded) != 2880 {
					t.Fatalf("invalid device Opus packet: %v", err)
				}
			}
			finish()
			select {
			case <-produced:
			case <-time.After(2 * time.Second):
				t.Fatal("producer did not finish")
			}
			select {
			case <-sent:
			case <-time.After(2 * time.Second):
				t.Fatal("playback did not finish")
			}
		})
	}
}

type blockingStreamingTTS struct{ cancelled chan struct{} }

func (*blockingStreamingTTS) Initialize() error                 { return nil }
func (*blockingStreamingTTS) Cleanup() error                    { return nil }
func (*blockingStreamingTTS) SetVoice(v string) (error, string) { return nil, v }
func (*blockingStreamingTTS) ToTTS(string) (string, error) {
	return "", fmt.Errorf("file path should not be called")
}
func (p *blockingStreamingTTS) StreamPCM(ctx context.Context, text string, emit func([]byte) error) (string, error) {
	if err := emit(make([]byte, 2880)); err != nil {
		return "", err
	}
	if text == "old" {
		<-ctx.Done()
		close(p.cancelled)
		return "", ctx.Err()
	}
	return "", nil
}

func TestInterruptCancelsBlockedSynthesisAndAllowsNextRound(t *testing.T) {
	h := realtimeHandler(t, "pcm")
	p := &blockingStreamingTTS{cancelled: make(chan struct{})}
	h.providers.tts = p
	oldProduced := make(chan struct{})
	go func() { h.processTTSTask("old", 1, 1); close(oldProduced) }()
	old := <-h.audioMessagesQueue
	oldSent := make(chan struct{})
	go func() { h.sendStreamingSpeech(old); close(oldSent) }()
	packets := h.conn.(*speechTestConn).packets
	for {
		packet := <-packets
		if packet.kind == 2 {
			break
		}
	}
	h.stopServerSpeak()
	select {
	case <-p.cancelled:
	case <-time.After(time.Second):
		t.Fatal("interruption left cloud synthesis blocked")
	}
	select {
	case <-oldProduced:
	case <-time.After(time.Second):
		t.Fatal("TTS worker remained blocked")
	}
	select {
	case <-oldSent:
	case <-time.After(time.Second):
		t.Fatal("audio sender remained blocked")
	}
	h.talkRound.Store(2)
	h.tts_last_text_index.Store(1)
	atomic.StoreInt32(&h.serverVoiceStop, 0)
	newProduced := make(chan struct{})
	go func() { h.processTTSTask("new", 1, 2); close(newProduced) }()
	var next audioMessage
	select {
	case next = <-h.audioMessagesQueue:
	case <-time.After(time.Second):
		t.Fatal("new round blocked behind old synthesis")
	}
	h.sendStreamingSpeech(next)
	select {
	case <-newProduced:
	case <-time.After(time.Second):
		t.Fatal("new synthesis did not finish")
	}
}

func TestSentenceEndDoesNotStopWhileLLMIsStillGenerating(t *testing.T) {
	h := realtimeHandler(t, "pcm")
	h.providers.tts = &blockingStreamingTTS{cancelled: make(chan struct{})}
	h.beginSpeechGeneration(1)
	done := make(chan struct{})
	go func() { h.processTTSTask("new", 1, 1); close(done) }()
	task := <-h.audioMessagesQueue
	h.sendStreamingSpeech(task)
	<-done
	packets := h.conn.(*speechTestConn).packets
	for len(packets) > 0 {
		packet := <-packets
		if packet.kind != 1 {
			continue
		}
		var state struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal(packet.data, &state); err != nil {
			t.Fatal(err)
		}
		if state.State == "stop" {
			t.Fatal("round stopped before LLM finished")
		}
	}
	if h.providers.asr.(*queuedASR).resets.Load() != 0 {
		t.Fatal("ASR reset while LLM is still generating")
	}
	h.endSpeechGeneration(1)
	select {
	case packet := <-packets:
		var state struct {
			State string `json:"state"`
		}
		json.Unmarshal(packet.data, &state)
		if state.State != "stop" || h.providers.asr.(*queuedASR).resets.Load() != 1 {
			t.Fatal("round did not stop after generation completed")
		}
	case <-time.After(time.Second):
		t.Fatal("missing final stop")
	}
}

func TestRealtimeSpeechStartCancelsPlaybackWithoutResettingASR(t *testing.T) {
	h := realtimeHandler(t, "pcm")
	h.clientListenMode = "realtime"
	ctx, release, ok := h.newSpeechContext(1)
	if !ok {
		t.Fatal("speech context unavailable")
	}
	defer release()
	h.OnAsrSpeechStart()
	if ctx.Err() != context.Canceled {
		t.Fatal("interim speech did not interrupt synthesis")
	}
	if h.providers.asr.(*queuedASR).resets.Load() != 0 {
		t.Fatal("barge-in reset ASR before the sentence finished")
	}
}
