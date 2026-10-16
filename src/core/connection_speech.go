package core

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"xiaozhi-server-go/src/core/providers"
	"xiaozhi-server-go/src/core/utils"
)

type synthesisResult struct {
	path string
	err  error
}
type streamingSpeech struct {
	ctx       context.Context
	cancel    context.CancelFunc
	release   func()
	chunks    chan []byte
	result    chan synthesisResult
	startedAt time.Time
}

func (h *ConnectionHandler) currentRound() int  { return int(h.talkRound.Load()) }
func (h *ConnectionHandler) lastTextIndex() int { return int(h.tts_last_text_index.Load()) }

// Realtime barge-in stops playback at interim speech, preserving the active ASR
// task until its final sentence. The LLM is never called with partial text.
func (h *ConnectionHandler) OnAsrSpeechStart() {
	if h.clientListenMode == "realtime" {
		h.stopServerSpeak()
		h.sendTTSMessage("stop", "", 0)
	}
}

func (h *ConnectionHandler) speechObsolete(round int) bool {
	if round != h.currentRound() || atomic.LoadInt32(&h.serverVoiceStop) == 1 {
		return true
	}
	select {
	case <-h.stopChan:
		return true
	default:
		return false
	}
}

func (h *ConnectionHandler) newSpeechContext(round int) (context.Context, func(), bool) {
	h.speechMu.Lock()
	defer h.speechMu.Unlock()
	if h.speechObsolete(round) {
		return nil, nil, false
	}
	parent := h.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	if h.speechCancels == nil {
		h.speechCancels = make(map[uint64]context.CancelFunc)
	}
	h.speechNext++
	id := h.speechNext
	h.speechCancels[id] = cancel
	var once sync.Once
	release := func() {
		once.Do(func() { cancel(); h.speechMu.Lock(); delete(h.speechCancels, id); h.speechMu.Unlock() })
	}
	return ctx, release, true
}

// newGenerationContext registers an LLM generation with cancelSpeech so that
// barge-in or a new round also stops the model stream and tool calls. Unlike
// newSpeechContext it does not consult serverVoiceStop, which is only cleared
// once the model starts responding.
func (h *ConnectionHandler) newGenerationContext(parent context.Context) (context.Context, func()) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	h.speechMu.Lock()
	defer h.speechMu.Unlock()
	if h.speechCancels == nil {
		h.speechCancels = make(map[uint64]context.CancelFunc)
	}
	h.speechNext++
	id := h.speechNext
	h.speechCancels[id] = cancel
	return ctx, func() { cancel(); h.speechMu.Lock(); delete(h.speechCancels, id); h.speechMu.Unlock() }
}

func (h *ConnectionHandler) cancelSpeech() {
	h.speechMu.Lock()
	for _, cancel := range h.speechCancels {
		cancel()
	}
	h.speechMu.Unlock()
}

// A sentence can finish playing before the LLM produces the next one. Emit
// the round-level stop only once both text generation and playback are done.
func (h *ConnectionHandler) beginSpeechGeneration(round int) {
	h.speechMu.Lock()
	defer h.speechMu.Unlock()
	if h.generationDepth == 0 && h.stoppedRound == round {
		h.stoppedRound = -1
		h.playedRound = -1
		h.playedIndex = 0
	}
	if h.generationRound != round {
		h.generationRound = round
		h.generationDepth = 0
	}
	h.generationDepth++
}
func (h *ConnectionHandler) endSpeechGeneration(round int) {
	h.speechMu.Lock()
	if h.generationRound == round && h.generationDepth > 0 {
		h.generationDepth--
	}
	h.speechMu.Unlock()
	h.maybeFinishSpeech(round)
}
func (h *ConnectionHandler) audioSentenceFinished(round, index int) {
	h.speechMu.Lock()
	if h.playedRound != round {
		h.playedRound = round
		h.playedIndex = index
	} else if index > h.playedIndex {
		h.playedIndex = index
	}
	h.speechMu.Unlock()
	h.maybeFinishSpeech(round)
}
func (h *ConnectionHandler) maybeFinishSpeech(round int) {
	h.speechMu.Lock()
	ready := !h.speechObsolete(round) && h.stoppedRound != round && h.playedRound == round && h.playedIndex == h.lastTextIndex() && h.playedIndex > 0 && !(h.generationRound == round && h.generationDepth > 0)
	if ready {
		h.stoppedRound = round
	}
	h.speechMu.Unlock()
	if !ready || h.speechObsolete(round) {
		return
	}
	h.sendTTSMessage("stop", "", h.lastTextIndex())
	if h.closeAfterChat {
		h.Close()
	} else if h.providers.asr != nil {
		h.clearSpeakStatus()
	}
}

func (h *ConnectionHandler) produceStreamingSpeech(provider providers.PCMStreamingTTS, text string, index, round int) {
	ctx, release, ok := h.newSpeechContext(round)
	if !ok {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &streamingSpeech{ctx: ctx, cancel: cancel, release: release, chunks: make(chan []byte, 4), result: make(chan synthesisResult, 1), startedAt: time.Now()}
	select {
	case h.audioMessagesQueue <- audioMessage{text: text, textIndex: index, round: round, stream: s}:
	case <-ctx.Done():
		cancel()
		release()
		return
	}
	relay := startChunkRelay(ctx, s.chunks)
	path, err := provider.StreamPCM(ctx, text, func(data []byte) error {
		if h.speechObsolete(round) {
			cancel()
			return context.Canceled
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relay.push(append([]byte(nil), data...))
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		h.LogError(fmt.Sprintf("流式 TTS 失败: %v", err))
	}
	if err == nil && ctx.Err() == nil && h.quickReplyCache != nil && utils.IsQuickReplyHit(text, h.config.QuickReplyWords) {
		if err := h.quickReplyCache.SaveCachedAudio(text, path); err != nil {
			h.LogError(fmt.Sprintf("保存快速回复缓存失败: %v", err))
		}
	}
	s.result <- synthesisResult{path: path, err: err}
	relay.finish()
}

// chunkRelay decouples cloud synthesis from playback pacing: the provider
// callback never blocks on the device, so the provider timeout covers only
// synthesis and the TTS worker can move on to the next sentence. Memory is
// bounded by the provider's maximum audio size per sentence.
type chunkRelay struct {
	mu     sync.Mutex
	queue  [][]byte
	done   bool
	signal chan struct{}
}

func startChunkRelay(ctx context.Context, out chan<- []byte) *chunkRelay {
	r := &chunkRelay{signal: make(chan struct{}, 1)}
	go func() {
		defer close(out)
		for {
			r.mu.Lock()
			var next []byte
			has := len(r.queue) > 0
			if has {
				next = r.queue[0]
				r.queue[0] = nil
				r.queue = r.queue[1:]
			}
			done := r.done
			r.mu.Unlock()
			if has {
				select {
				case out <- next:
					continue
				case <-ctx.Done():
					return
				}
			}
			if done {
				return
			}
			select {
			case <-r.signal:
			case <-ctx.Done():
				return
			}
		}
	}()
	return r
}

func (r *chunkRelay) push(data []byte) {
	r.mu.Lock()
	r.queue = append(r.queue, data)
	r.mu.Unlock()
	r.wake()
}

func (r *chunkRelay) finish() {
	r.mu.Lock()
	r.done = true
	r.mu.Unlock()
	r.wake()
}

func (r *chunkRelay) wake() {
	select {
	case r.signal <- struct{}{}:
	default:
	}
}

func (h *ConnectionHandler) discardStreamingResult(s *streamingSpeech) {
	defer s.release()
	result := <-s.result
	h.deleteAudioFileIfNeeded(result.path, "丢弃流式合成结果")
}

func waitSpeech(ctx context.Context, until time.Time) error {
	delay := time.Until(until)
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (h *ConnectionHandler) sendStreamingSpeech(task audioMessage) {
	s := task.stream
	consumed := false
	started := false
	defer func() {
		s.cancel()
		if consumed {
			s.release()
		} else {
			go h.discardStreamingResult(s)
		}
		if h.providers.asr != nil {
			h.providers.asr.ResetStartListenTime()
		}
		if started && !h.speechObsolete(task.round) {
			h.sendTTSMessage("sentence_end", task.text, task.textIndex)
		}
		if !h.speechObsolete(task.round) {
			h.audioSentenceFinished(task.round, task.textIndex)
		}
	}()
	if h.speechObsolete(task.round) {
		return
	}
	frameTime := time.Duration(h.serverAudioFrameDuration) * time.Millisecond
	var playbackEnd time.Time
	var capture [][]byte
	err := utils.StreamPCMFrames(s.ctx, s.chunks, h.serverAudioFormat, h.serverAudioFrameDuration, func(packet []byte) error {
		if h.speechObsolete(task.round) {
			return context.Canceled
		}
		// At most three frames buffered, matching the existing file sender.
		if !playbackEnd.IsZero() {
			if err := waitSpeech(s.ctx, playbackEnd.Add(-2*frameTime)); err != nil {
				return err
			}
		}
		if err := s.ctx.Err(); err != nil {
			return err
		}
		if h.speechObsolete(task.round) {
			return context.Canceled
		}
		if !started {
			if err := h.sendTTSMessage("sentence_start", task.text, task.textIndex); err != nil {
				return err
			}
			started = true
			h.LogInfo(fmt.Sprintf("[TTS] [流式首帧 %d] 合成开始至设备首帧 %s", task.textIndex, time.Since(s.startedAt)))
		}
		if err := h.conn.WriteMessage(2, packet); err != nil {
			return err
		}
		if h.config.SaveTTSAudio && h.serverAudioFormat == "opus" {
			capture = append(capture, append([]byte(nil), packet...))
		}
		now := time.Now()
		if now.After(playbackEnd) {
			playbackEnd = now
		}
		playbackEnd = playbackEnd.Add(frameTime)
		return nil
	})
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			h.LogError(fmt.Sprintf("流式音频发送失败: %v", err))
		}
		return
	}
	result := <-s.result
	consumed = true
	h.deleteAudioFileIfNeeded(result.path, "流式音频发送完成")
	if result.err != nil {
		return
	}
	if len(capture) > 0 {
		if _, err := utils.SaveOpusPackets(filepath.Join(filepath.Dir(result.path), "opus_packets"), h.deviceID, h.sessionID, 24000, 1, h.serverAudioFrameDuration, capture); err != nil {
			h.LogError(fmt.Sprintf("保存流式 Opus 抓包失败: %v", err))
		}
	}
	if !playbackEnd.IsZero() {
		_ = waitSpeech(s.ctx, playbackEnd)
	}
}
