// Package dashscope implements Qwen3-TTS Realtime (not Paraformer/CosyVoice).
package dashscope

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"xiaozhi-server-go/src/configs"
	"xiaozhi-server-go/src/core/providers"
	"xiaozhi-server-go/src/core/providers/tts"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const maxAudioBytes = 32 * 1024 * 1024

var _ providers.TTSProvider = (*Provider)(nil)

type Provider struct {
	*tts.BaseProvider
	mu     sync.Mutex
	active map[string]context.CancelFunc
	files  map[string]struct{}
	closed bool
}

type event struct {
	Type     string                         `json:"type"`
	Delta    string                         `json:"delta"`
	Error    struct{ Code, Message string } `json:"error"`
	Response struct {
		Status        string `json:"status"`
		StatusDetails struct {
			Error struct{ Code, Message string } `json:"error"`
		} `json:"status_details"`
	} `json:"response"`
}

func init() { tts.Register("dashscope", NewProvider) }

func NewProvider(cfg *tts.Config, deleteFile bool) (tts.Provider, error) {
	if cfg == nil {
		return nil, fmt.Errorf("DashScope TTS config is required")
	}
	copy := *cfg
	copy.Type = "dashscope"
	copy.SupportedVoices = append([]configs.VoiceInfo(nil), cfg.SupportedVoices...)
	defaults := configs.DefaultDashScopeTTS()
	if copy.APIKey = strings.TrimSpace(copy.APIKey); copy.APIKey == "" {
		copy.APIKey = strings.TrimSpace(os.Getenv("DASHSCOPE_API_KEY"))
	}
	if copy.APIKey == "" {
		return nil, fmt.Errorf("DashScope TTS requires api_key or DASHSCOPE_API_KEY")
	}
	if copy.URL == "" {
		copy.URL = defaults.URL
	}
	if copy.Model == "" {
		copy.Model = defaults.Model
	}
	if copy.OutputDir == "" {
		copy.OutputDir = defaults.OutputDir
	}
	if copy.Format == "" {
		copy.Format = "wav"
	}
	if copy.Format != "wav" {
		return nil, fmt.Errorf("DashScope TTS requires format wav (PCM is wrapped locally)")
	}
	if copy.SampleRate == 0 {
		copy.SampleRate = 24000
	}
	if copy.SampleRate != 24000 {
		return nil, fmt.Errorf("DashScope TTS playback currently requires sample_rate 24000")
	}
	if copy.TimeoutSeconds == 0 {
		copy.TimeoutSeconds = 60
	}
	if copy.TimeoutSeconds < 1 || copy.TimeoutSeconds > 600 {
		return nil, fmt.Errorf("timeout_seconds must be between 1 and 600")
	}
	u, err := url.Parse(copy.URL)
	if err != nil || u.Host == "" || (u.Scheme != "wss" && u.Scheme != "ws") || u.User != nil {
		return nil, fmt.Errorf("DashScope TTS url must be ws:// or wss:// without embedded credentials")
	}
	query := u.Query()
	query.Set("model", copy.Model)
	u.RawQuery = query.Encode()
	copy.URL = u.String()
	if len(copy.SupportedVoices) == 0 {
		if copy.Model != defaults.Model {
			return nil, fmt.Errorf("configure supported_voices for TTS model %q", copy.Model)
		}
		copy.SupportedVoices = configs.DashScopeVoices()
	}
	seen := make(map[string]bool)
	for _, voice := range copy.SupportedVoices {
		if voice.Name == "" || seen[voice.Name] {
			return nil, fmt.Errorf("supported_voices must contain unique non-empty API names")
		}
		seen[voice.Name] = true
	}
	if copy.Voice == "" {
		copy.Voice = "Cherry"
	}
	ok, voice, err := tts.IsSupportedVoice(copy.Voice, copy.SupportedVoices)
	if err != nil || !ok {
		return nil, fmt.Errorf("DashScope TTS voice %q is not in supported_voices", copy.Voice)
	}
	copy.Voice = voice
	return &Provider{BaseProvider: tts.NewBaseProvider(&copy, deleteFile), active: make(map[string]context.CancelFunc), files: make(map[string]struct{})}, nil
}

// Config returns a snapshot; synthesis and voice changes can run concurrently.
func (p *Provider) Config() *tts.Config {
	p.mu.Lock()
	defer p.mu.Unlock()
	copy := *p.BaseProvider.Config()
	copy.SupportedVoices = append([]configs.VoiceInfo(nil), copy.SupportedVoices...)
	return &copy
}

func (p *Provider) SetVoice(voice string) (error, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cfg := p.BaseProvider.Config()
	ok, name, err := tts.IsSupportedVoice(voice, cfg.SupportedVoices)
	if err != nil || !ok {
		return fmt.Errorf("unsupported DashScope voice %q", voice), ""
	}
	cfg.Voice = name
	return nil, name
}

func (p *Provider) ToTTS(text string) (string, error) {
	return p.ToTTSWithContext(context.Background(), text)
}

// ToTTSWithContext consumes cloud audio deltas but returns a complete WAV file
// to preserve the existing sentence-based playback contract.
func (p *Provider) ToTTSWithContext(ctx context.Context, text string) (string, error) {
	return p.StreamPCM(ctx, text, nil)
}

// StreamPCM delivers PCM16 mono 24kHz deltas as soon as they arrive, while
// retaining the complete WAV result for recording/cache compatibility. A slow
// callback applies backpressure; cancellation closes the cloud connection.
func (p *Provider) StreamPCM(ctx context.Context, text string, emit func([]byte) error) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("DashScope TTS text is empty")
	}
	cfg := p.Config()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()
	id := uuid.NewString()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return "", fmt.Errorf("DashScope TTS provider is closed")
	}
	p.active[id] = cancel
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.active, id); p.mu.Unlock() }()
	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, cfg.URL, http.Header{"Authorization": []string{"Bearer " + cfg.APIKey}})
	if err != nil {
		if resp != nil {
			if resp.Body != nil {
				resp.Body.Close()
			}
			return "", fmt.Errorf("DashScope TTS handshake HTTP %d", resp.StatusCode)
		}
		return "", fmt.Errorf("DashScope TTS connect: %w", err)
	}
	defer conn.Close()
	conn.SetReadLimit(2 * 1024 * 1024)
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-stopWatch:
		}
	}()
	read := func() (event, error) {
		var e event
		deadline, _ := ctx.Deadline()
		conn.SetReadDeadline(deadline)
		if err := conn.ReadJSON(&e); err != nil {
			if ctx.Err() != nil {
				return e, ctx.Err()
			}
			return e, fmt.Errorf("DashScope TTS read before session.finished: %w", err)
		}
		if e.Type == "error" {
			return e, fmt.Errorf("DashScope TTS %s: %s", e.Error.Code, strings.ReplaceAll(e.Error.Message, cfg.APIKey, "<redacted>"))
		}
		if e.Type == "response.done" && e.Response.Status != "" && e.Response.Status != "completed" {
			return e, fmt.Errorf("DashScope TTS response %s: %s", e.Response.Status, strings.ReplaceAll(e.Response.StatusDetails.Error.Message, cfg.APIKey, "<redacted>"))
		}
		return e, nil
	}
	send := func(kind string, fields map[string]any) error {
		fields["event_id"] = "event_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		fields["type"] = kind
		deadline, _ := ctx.Deadline()
		conn.SetWriteDeadline(deadline)
		if err := conn.WriteJSON(fields); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("DashScope TTS send %s: %w", kind, err)
		}
		return nil
	}
	e, err := read()
	if err != nil {
		return "", err
	}
	if e.Type != "session.created" {
		return "", fmt.Errorf("DashScope TTS expected session.created, got %q", e.Type)
	}
	if err := send("session.update", map[string]any{"session": map[string]any{"voice": cfg.Voice, "mode": "server_commit", "response_format": "pcm", "sample_rate": cfg.SampleRate}}); err != nil {
		return "", err
	}
	e, err = read()
	if err != nil {
		return "", err
	}
	if e.Type != "session.updated" {
		return "", fmt.Errorf("DashScope TTS expected session.updated, got %q", e.Type)
	}
	for _, chunk := range splitText(text) {
		if err := send("input_text_buffer.append", map[string]any{"text": chunk}); err != nil {
			return "", err
		}
	}
	if err := send("session.finish", map[string]any{}); err != nil {
		return "", err
	}
	var pcm bytes.Buffer
	for {
		e, err := read()
		if err != nil {
			return "", err
		}
		switch e.Type {
		case "response.audio.delta":
			data, err := base64.StdEncoding.DecodeString(e.Delta)
			if err != nil {
				return "", fmt.Errorf("DashScope TTS invalid base64 audio: %w", err)
			}
			if pcm.Len()+len(data) > maxAudioBytes {
				return "", fmt.Errorf("DashScope TTS audio exceeds %d bytes", maxAudioBytes)
			}
			pcm.Write(data)
			if emit != nil && len(data) > 0 {
				if err := emit(data); err != nil {
					return "", err
				}
			}
		case "session.finished":
			if err := ctx.Err(); err != nil {
				return "", err
			}
			if pcm.Len() == 0 || pcm.Len()%2 != 0 {
				return "", fmt.Errorf("DashScope TTS returned empty or incomplete PCM16 audio")
			}
			return p.saveWAV(cfg.OutputDir, pcm.Bytes())
		}
	}
}

func (p *Provider) saveWAV(dir string, pcm []byte) (string, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "dashscope_tts_*.wav")
	if err != nil {
		return "", err
	}
	keep := false
	defer func() {
		f.Close()
		if !keep {
			os.Remove(f.Name())
		}
	}()
	header := make([]byte, 44)
	copy(header, "RIFF")
	binary.LittleEndian.PutUint32(header[4:], uint32(36+len(pcm)))
	copy(header[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(header[16:], 16)
	binary.LittleEndian.PutUint16(header[20:], 1)
	binary.LittleEndian.PutUint16(header[22:], 1)
	binary.LittleEndian.PutUint32(header[24:], 24000)
	binary.LittleEndian.PutUint32(header[28:], 48000)
	binary.LittleEndian.PutUint16(header[32:], 2)
	binary.LittleEndian.PutUint16(header[34:], 16)
	copy(header[36:], "data")
	binary.LittleEndian.PutUint32(header[40:], uint32(len(pcm)))
	if _, err := f.Write(header); err != nil {
		return "", err
	}
	if _, err := f.Write(pcm); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return "", context.Canceled
	}
	p.files[f.Name()] = struct{}{}
	keep = true
	return f.Name(), nil
}

func (p *Provider) Cleanup() error {
	p.mu.Lock()
	p.closed = true
	var cancels []context.CancelFunc
	for _, cancel := range p.active {
		cancels = append(cancels, cancel)
	}
	var paths []string
	if p.DeleteFile() {
		for path := range p.files {
			paths = append(paths, path)
		}
		p.files = make(map[string]struct{})
	}
	p.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// Preserve punctuation and Unicode while bounding each append to 50 runes.
func splitText(text string) []string {
	var chunks []string
	var chunk []rune
	for _, r := range text {
		chunk = append(chunk, r)
		if len(chunk) == 50 || strings.ContainsRune("，。！？；、,.!?;", r) {
			chunks = append(chunks, string(chunk))
			chunk = nil
		}
	}
	if len(chunk) > 0 {
		chunks = append(chunks, string(chunk))
	}
	return chunks
}
