// Package dashscope implements Alibaba Cloud Model Studio Paraformer ASR.
package dashscope

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"xiaozhi-server-go/src/core/providers"
	"xiaozhi-server-go/src/core/providers/asr"
	"xiaozhi-server-go/src/core/utils"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const defaultURL = "wss://dashscope.aliyuncs.com/api-ws/v1/inference"

var errUtteranceComplete = errors.New("DashScope live utterance already completed")

var _ providers.ASRProvider = (*Provider)(nil)

type Provider struct {
	*asr.BaseProvider
	apiKey, endpoint, model string
	parameters              map[string]any
	sampleRate              int
	timeout                 time.Duration
	logger                  *utils.Logger
	startMu                 sync.Mutex
	mu                      sync.Mutex
	current                 *session
	listener                providers.AsrEventListener
	mode                    string
	paused                  bool // completed live utterance: ignore its queued tail
	startCancel             context.CancelFunc
}

// Every recognition owns its connection, results and completion channels. An
// old reader can never close or overwrite a subsequently started recognition.
type session struct {
	conn        *websocket.Conn
	id          string
	started     chan struct{}
	done        chan struct{}
	once        sync.Once
	writeMu     sync.Mutex
	ending      bool
	mu          sync.Mutex
	err         error
	texts       map[string]string
	order       []string
	synchronous bool
	manual      bool
	sampleRate  int
}

type event struct {
	Header struct {
		Event        string `json:"event"`
		TaskID       string `json:"task_id"`
		ErrorCode    string `json:"error_code"`
		ErrorMessage string `json:"error_message"`
	} `json:"header"`
	Payload struct {
		Output struct {
			Sentence struct {
				ID        *int   `json:"sentence_id"`
				Begin     int    `json:"begin_time"`
				Text      string `json:"text"`
				End       bool   `json:"sentence_end"`
				Heartbeat bool   `json:"heartbeat"`
			} `json:"sentence"`
		} `json:"output"`
	} `json:"payload"`
}

func init() {
	asr.Register("dashscope", func(cfg *asr.Config, deleteFile bool, logger *utils.Logger) (asr.Provider, error) {
		return NewProvider(cfg, deleteFile, logger)
	})
}

func NewProvider(cfg *asr.Config, deleteFile bool, logger *utils.Logger) (*Provider, error) {
	if cfg == nil {
		return nil, fmt.Errorf("DashScope ASR config is required")
	}
	key := stringValue(cfg.Data, "api_key", "")
	if key == "" {
		key = strings.TrimSpace(os.Getenv("DASHSCOPE_API_KEY"))
	}
	if key == "" {
		return nil, fmt.Errorf("DashScope ASR requires api_key or DASHSCOPE_API_KEY")
	}
	endpoint := stringValue(cfg.Data, "asr_url", defaultURL)
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "ws" && u.Scheme != "wss") || u.User != nil {
		return nil, fmt.Errorf("DashScope asr_url must be a ws:// or wss:// URL without embedded credentials")
	}
	// The connection layer already decodes Opus into PCM16. Do not label those
	// raw samples as WAV or send device Opus packets directly to this API.
	if format := stringValue(cfg.Data, "format", "pcm"); format != "pcm" {
		return nil, fmt.Errorf("DashScope device ASR requires format pcm")
	}
	rate, err := positiveInt(cfg.Data, "sample_rate", 16000)
	if err != nil {
		return nil, err
	}
	seconds, err := positiveInt(cfg.Data, "timeout_seconds", 30)
	if err != nil || seconds > 600 {
		return nil, fmt.Errorf("timeout_seconds must be an integer from 1 to 600")
	}
	model := stringValue(cfg.Data, "model", "paraformer-realtime-v2")
	params := map[string]any{"format": "pcm", "sample_rate": rate}
	if model == "paraformer-realtime-v2" {
		params["semantic_punctuation_enabled"] = false
		params["max_sentence_silence"] = 300
	}
	for _, name := range []string{"phrase_id", "disfluency_removal_enabled", "diarization_enabled", "speaker_count", "timestamp_alignment_enabled", "special_word_filter", "audio_event_detection_enabled", "semantic_punctuation_enabled", "max_sentence_silence"} {
		if value, ok := cfg.Data[name]; ok {
			params[name] = value
		}
	}
	if value, exists := cfg.Data["max_sentence_silence"]; exists {
		ms, err := positiveInt(map[string]any{"max_sentence_silence": value}, "max_sentence_silence", 300)
		if err != nil || ms < 200 || ms > 6000 {
			return nil, fmt.Errorf("max_sentence_silence must be 200–6000 ms")
		}
	}
	if hints, ok := cfg.Data["language_hints"]; ok {
		var values []string
		switch typed := hints.(type) {
		case []string:
			values = append(values, typed...)
		case []any:
			for _, item := range typed {
				value, ok := item.(string)
				if !ok {
					return nil, fmt.Errorf("language_hints must be a string array")
				}
				values = append(values, value)
			}
		default:
			return nil, fmt.Errorf("language_hints must be a string array")
		}
		if len(values) > 0 {
			if model != "paraformer-realtime-v2" {
				return nil, fmt.Errorf("language_hints requires paraformer-realtime-v2")
			}
			params["language_hints"] = values
		}
	}
	p := &Provider{BaseProvider: asr.NewBaseProvider(cfg, deleteFile), apiKey: key, endpoint: endpoint, model: model, parameters: params, sampleRate: rate, timeout: time.Duration(seconds) * time.Second, logger: logger, mode: "auto"}
	p.InitAudioProcessing()
	return p, nil
}

func (p *Provider) SetListener(listener providers.AsrEventListener) {
	p.mu.Lock()
	p.listener = listener
	p.mu.Unlock()
}

// ConfigureInput is optional for ASR providers; the connection calls it with
// actual decoded audio parameters and listen mode before streaming starts.
func (p *Provider) ConfigureInput(rate, channels int, mode string) error {
	if rate <= 0 || channels != 1 {
		return fmt.Errorf("DashScope ASR requires mono PCM16 with a positive sample rate")
	}
	if mode != "manual" && mode != "auto" && mode != "realtime" {
		return fmt.Errorf("unsupported ASR listen mode %q", mode)
	}
	p.Reset()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sampleRate, p.mode = rate, mode
	return nil
}

// Prepare starts cloud setup on listen.start, overlapping it with microphone
// capture. The audio queue calls this before forwarding that utterance's frames.
func (p *Provider) Prepare(ctx context.Context) error { return p.StartStreaming(ctx) }

func (p *Provider) start(ctx context.Context, synchronous bool) (*session, error) {
	p.startMu.Lock()
	defer p.startMu.Unlock()
	p.mu.Lock()
	old := p.current
	rate, mode := p.sampleRate, p.mode
	paused := p.paused
	p.mu.Unlock()
	if paused && !synchronous {
		return nil, errUtteranceComplete
	}
	if old != nil {
		select {
		case <-old.done:
		default:
			if synchronous {
				return nil, fmt.Errorf("DashScope recognition already running")
			}
			return old, nil
		}
	}
	startCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	p.mu.Lock()
	p.startCancel = cancel
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.startCancel = nil; p.mu.Unlock() }()
	headers := http.Header{"Authorization": []string{"bearer " + p.apiKey}}
	conn, resp, err := websocket.DefaultDialer.DialContext(startCtx, p.endpoint, headers)
	if err != nil {
		if resp != nil {
			if resp.Body != nil {
				resp.Body.Close()
			}
			return nil, fmt.Errorf("DashScope ASR handshake HTTP %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("DashScope ASR connect: %w", err)
	}
	conn.SetReadLimit(2 * 1024 * 1024)
	s := &session{conn: conn, id: strings.ReplaceAll(uuid.NewString(), "-", ""), started: make(chan struct{}), done: make(chan struct{}), texts: make(map[string]string), synchronous: synchronous, manual: mode == "manual", sampleRate: rate}
	params := make(map[string]any, len(p.parameters))
	for key, value := range p.parameters {
		params[key] = value
	}
	params["sample_rate"] = rate
	payload := map[string]any{"task_group": "audio", "task": "asr", "function": "recognition", "model": p.model, "parameters": params, "input": map[string]any{}}
	if err := s.writeJSON("run-task", payload, p.timeout); err != nil {
		s.complete(err)
		return nil, err
	}
	p.mu.Lock()
	p.current = s
	p.mu.Unlock()
	go p.readLoop(s)
	select {
	case <-s.started:
		return s, nil
	case <-s.done:
		return nil, s.resultError()
	case <-startCtx.Done():
		s.complete(startCtx.Err())
		return nil, startCtx.Err()
	}
}

func (p *Provider) StartStreaming(ctx context.Context) error {
	_, err := p.start(ctx, false)
	return err
}
func (p *Provider) AddAudio(data []byte) error {
	return p.AddAudioWithContext(context.Background(), data)
}
func (p *Provider) AddAudioWithContext(ctx context.Context, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	if len(data)%2 != 0 {
		return fmt.Errorf("PCM16 audio must contain complete 16-bit samples")
	}
	p.mu.Lock()
	paused := p.paused
	p.mu.Unlock()
	if paused {
		return nil
	}
	s, err := p.start(ctx, false)
	if errors.Is(err, errUtteranceComplete) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.writeAudio(data, p.timeout); err != nil {
		p.fail(s, err)
		return err
	}
	return nil
}

func (p *Provider) SendLastAudio(data []byte) error {
	p.mu.Lock()
	s := p.current
	p.mu.Unlock()
	if s == nil {
		if len(data) == 0 {
			return nil
		}
		var err error
		s, err = p.start(context.Background(), false)
		if err != nil {
			return err
		}
	}
	if len(data) > 0 {
		if err := s.writeAudio(data, p.timeout); err != nil {
			return err
		}
	}
	return s.finishInput(p.timeout)
}

// Transcribe accepts raw PCM16 mono at the configured sample rate, matching the
// streaming interface. Audio is paced in 100 ms frames and respects ctx.
func (p *Provider) Transcribe(ctx context.Context, audio []byte) (string, error) {
	if len(audio)%2 != 0 {
		return "", fmt.Errorf("PCM16 audio must contain complete samples")
	}
	s, err := p.start(ctx, true)
	if err != nil {
		return "", err
	}
	defer s.complete(context.Canceled)
	// Closing this session interrupts an in-flight socket write as well as a
	// blocked read; completion of older sessions never affects a newer one.
	go func() {
		select {
		case <-ctx.Done():
			s.complete(ctx.Err())
		case <-s.done:
		}
	}()
	frame := s.sampleRate / 10 * 2
	if frame < 2 {
		frame = 2
	}
	for offset := 0; offset < len(audio); offset += frame {
		end := offset + frame
		if end > len(audio) {
			end = len(audio)
		}
		if err := s.writeAudio(audio[offset:end], p.timeout); err != nil {
			return "", err
		}
		if end < len(audio) {
			timer := time.NewTimer(time.Duration(end-offset) * time.Second / time.Duration(s.sampleRate*2))
			select {
			case <-ctx.Done():
				timer.Stop()
				return "", ctx.Err()
			case <-s.done:
				timer.Stop()
				return s.result()
			case <-timer.C:
			}
		}
	}
	if err := s.finishInput(p.timeout); err != nil {
		return "", err
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-s.done:
		return s.result()
	}
}

func (p *Provider) readLoop(s *session) {
	defer s.complete(fmt.Errorf("DashScope ASR connection ended without task-finished"))
	started := false
	speechSignaled := false
	for {
		s.conn.SetReadDeadline(time.Now().Add(p.timeout))
		_, data, err := s.conn.ReadMessage()
		if err != nil {
			p.fail(s, fmt.Errorf("DashScope ASR read: %w", err))
			return
		}
		var e event
		if err := json.Unmarshal(data, &e); err != nil {
			p.fail(s, fmt.Errorf("DashScope ASR invalid event: %w", err))
			return
		}
		if e.Header.TaskID != "" && e.Header.TaskID != s.id {
			continue
		}
		switch e.Header.Event {
		case "task-started":
			if !started {
				started = true
				close(s.started)
			}
		case "task-failed":
			p.fail(s, fmt.Errorf("DashScope ASR task %s failed (%s): %s", s.id, e.Header.ErrorCode, strings.ReplaceAll(e.Header.ErrorMessage, p.apiKey, "<redacted>")))
			return
		case "result-generated":
			if !started {
				p.fail(s, fmt.Errorf("DashScope ASR result before task-started"))
				return
			}
			sentence := e.Payload.Output.Sentence
			if !s.synchronous && !speechSignaled && !sentence.Heartbeat && strings.TrimSpace(sentence.Text) != "" {
				speechSignaled = true
				p.mu.Lock()
				listener, current := p.listener, p.current
				p.mu.Unlock()
				if listener, ok := listener.(providers.AsrSpeechStartListener); ok && current == s {
					listener.OnAsrSpeechStart()
				}
			}
			if sentence.Heartbeat || !sentence.End || strings.TrimSpace(sentence.Text) == "" {
				continue
			}
			key := "time:" + strconv.Itoa(sentence.Begin)
			if sentence.ID != nil {
				key = "id:" + strconv.Itoa(*sentence.ID)
			}
			s.mu.Lock()
			if _, exists := s.texts[key]; !exists {
				s.order = append(s.order, key)
			}
			s.texts[key] = sentence.Text
			s.mu.Unlock()
			if !s.synchronous && !s.manual {
				p.pause(s)
				if err := s.finishInput(p.timeout); err != nil {
					p.fail(s, err)
					return
				}
				s.complete(nil)
				p.notify(s)
				return
			}
		case "task-finished":
			if !started {
				p.fail(s, fmt.Errorf("DashScope ASR finished before task-started"))
				return
			}
			if !s.synchronous {
				p.pause(s)
			}
			s.complete(nil)
			if !s.synchronous {
				p.notify(s)
			}
			return
		}
	}
}

func (p *Provider) pause(s *session) {
	p.mu.Lock()
	if p.current == s {
		p.paused = true
	}
	p.mu.Unlock()
}

func (p *Provider) notify(s *session) {
	text, err := s.result()
	p.mu.Lock()
	listener, current := p.listener, p.current
	p.mu.Unlock()
	if current == s && listener != nil && err == nil && strings.TrimSpace(text) != "" {
		listener.OnAsrResult(text, true)
	}
}

func (p *Provider) fail(s *session, err error) {
	select {
	case <-s.done:
		return
	default:
	}
	s.complete(err)
	if p.logger != nil {
		p.logger.Error("[ASR] [dashscope] %v", err)
	}
}

func (p *Provider) Reset() error {
	// Cancel a pending handshake/task-start before waiting for its startup lock.
	p.mu.Lock()
	if p.startCancel != nil {
		p.startCancel()
	}
	p.mu.Unlock()
	p.startMu.Lock()
	defer p.startMu.Unlock()
	p.mu.Lock()
	old := p.current
	p.current = nil
	p.paused = false
	p.mu.Unlock()
	if old != nil {
		old.complete(context.Canceled)
	}
	return nil
}
func (p *Provider) CloseConnection() error { return p.Reset() }
func (p *Provider) Cleanup() error         { return p.Reset() }

func (s *session) complete(err error) {
	s.once.Do(func() { s.mu.Lock(); s.err = err; s.mu.Unlock(); s.conn.Close(); close(s.done) })
}
func (s *session) result() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var text strings.Builder
	for _, key := range s.order {
		text.WriteString(s.texts[key])
	}
	return text.String(), s.err
}
func (s *session) resultError() error {
	_, err := s.result()
	if err == nil {
		err = fmt.Errorf("DashScope ASR task finished before audio was sent")
	}
	return err
}
func (s *session) writeJSON(action string, payload map[string]any, timeout time.Duration) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.writeJSONLocked(action, payload, timeout)
}
func (s *session) writeJSONLocked(action string, payload map[string]any, timeout time.Duration) error {
	s.conn.SetWriteDeadline(time.Now().Add(timeout))
	return s.conn.WriteJSON(map[string]any{"header": map[string]any{"action": action, "task_id": s.id, "streaming": "duplex"}, "payload": payload})
}
func (s *session) writeAudio(data []byte, timeout time.Duration) error {
	if len(data)%2 != 0 {
		return fmt.Errorf("PCM16 audio must contain complete samples")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.ending {
		return fmt.Errorf("DashScope ASR task is finishing")
	}
	select {
	case <-s.done:
		return s.resultError()
	default:
	}
	s.conn.SetWriteDeadline(time.Now().Add(timeout))
	return s.conn.WriteMessage(websocket.BinaryMessage, data)
}
func (s *session) finishInput(timeout time.Duration) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.ending {
		return nil
	}
	s.ending = true
	return s.writeJSONLocked("finish-task", map[string]any{"input": map[string]any{}}, timeout)
}

func stringValue(data map[string]any, key, fallback string) string {
	value, _ := data[key].(string)
	if value = strings.TrimSpace(value); value == "" {
		return fallback
	}
	return value
}
func positiveInt(data map[string]any, key string, fallback int) (int, error) {
	value, ok := data[key]
	if !ok {
		return fallback, nil
	}
	var number int
	switch value := value.(type) {
	case int:
		number = value
	case float64:
		number = int(value)
		if float64(number) != value {
			return 0, fmt.Errorf("%s must be a positive integer", key)
		}
	default:
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	if number <= 0 || number > 192000 {
		return 0, fmt.Errorf("%s must be a positive integer <= 192000", key)
	}
	return number, nil
}
