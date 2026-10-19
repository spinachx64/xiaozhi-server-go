package core

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"xiaozhi-server-go/src/configs"
	"xiaozhi-server-go/src/core/chat"
	"xiaozhi-server-go/src/core/delegate"
)

const (
	delegateIdlePoll      = 300 * time.Millisecond
	delegateIdleStable    = 3 // 连续空闲次数，避免插在两句之间播报
	delegateSpeakMaxRunes = 600
	delegateMemoryRunes   = 2000
)

var (
	delegateOnce sync.Once
	delegateMgr  *delegate.Manager
	delegateErr  error

	reCodeFence = regexp.MustCompile("(?s)```.*?```")
)

// globalDelegateManager 按首次连接时的配置创建进程级任务管理器。
// 任务独立于 WebSocket 连接存在，设备空闲断线后重连仍能拿到结果。
func globalDelegateManager(cfg *configs.Config) (*delegate.Manager, error) {
	delegateOnce.Do(func() {
		if cfg == nil || strings.TrimSpace(cfg.Delegate.URL) == "" {
			delegateErr = errors.New("delegate.url 未配置")
			return
		}
		token, err := configs.ResolveAPIKey(cfg.Delegate.Token, cfg.Delegate.EnvKey)
		if err != nil {
			delegateErr = fmt.Errorf("delegate token: %w", err)
			return
		}
		client, err := delegate.NewClient(cfg.Delegate.URL, token)
		if err != nil {
			delegateErr = err
			return
		}
		delegateMgr = delegate.NewManager(client, time.Duration(cfg.Delegate.TimeoutSeconds)*time.Second, cfg.Delegate.MaxConcurrent)
		delegateMgr.SetLogger(func(msg string) { fmt.Println("[后台任务]", msg) })
	})
	return delegateMgr, delegateErr
}

func delegateEnabled(cfg *configs.Config) bool {
	if cfg == nil {
		return false
	}
	for _, f := range cfg.LocalMCPFun {
		if f.Name == "delegate" && f.Enabled {
			return true
		}
	}
	return false
}

func (h *ConnectionHandler) delegateKey() string {
	if h.deviceID != "" {
		return h.deviceID
	}
	return h.sessionID
}

func (h *ConnectionHandler) delegateManager() (*delegate.Manager, error) {
	if h.delegates != nil {
		return h.delegates, nil
	}
	m, err := globalDelegateManager(h.config)
	if err == nil {
		h.delegates = m
	}
	return m, err
}

// startDelegateDelivery 绑定设备并启动结果播报协程，返回的函数用于解绑。
func (h *ConnectionHandler) startDelegateDelivery() func() {
	if h.delegates == nil && !delegateEnabled(h.config) {
		return nil
	}
	m, err := h.delegateManager()
	if err != nil {
		h.LogError(fmt.Sprintf("[后台任务] 未启用: %v", err))
		return nil
	}
	wake := make(chan struct{}, 1)
	key := h.delegateKey()
	detach := m.Attach(key, func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	})
	go h.runDelegateDelivery(m, key, wake)
	return detach
}

func (h *ConnectionHandler) runDelegateDelivery(m *delegate.Manager, key string, wake <-chan struct{}) {
	for {
		select {
		case <-h.stopChan:
			return
		case <-wake:
		}
		for m.HasPending(key) {
			if !h.waitDelegateIdle() {
				return
			}
			r, ok := m.TakePending(key)
			if !ok {
				break
			}
			h.announceDelegateResult(r)
			m.Delivered(key, r.TaskID)
		}
	}
}

// deviceIdle 判断当前没有生成中的回复，也没有待播放的语音。
func (h *ConnectionHandler) deviceIdle() bool {
	if len(h.ttsQueue) > 0 || len(h.audioMessagesQueue) > 0 {
		return false
	}
	round := h.currentRound()
	h.speechMu.Lock()
	defer h.speechMu.Unlock()
	if h.generationRound == round && h.generationDepth > 0 {
		return false
	}
	return h.lastTextIndex() == -1 || h.stoppedRound == round
}

func (h *ConnectionHandler) waitDelegateIdle() bool {
	ticker := time.NewTicker(delegateIdlePoll)
	defer ticker.Stop()
	stable := 0
	for {
		select {
		case <-h.stopChan:
			return false
		case <-ticker.C:
		}
		round := h.currentRound()
		if h.deviceIdle() {
			stable++
		} else {
			stable = 0
		}
		if stable >= delegateIdleStable && round == h.currentRound() {
			return true
		}
	}
}

// announceDelegateResult 以服务端主动发起的新轮次播报结果。用户此时开口会
// 开启新轮次，播报随之被打断，结果已写入对话历史，可以再追问。
func (h *ConnectionHandler) announceDelegateResult(r delegate.Result) {
	speech, memory := delegateAnnouncement(r)
	h.LogInfo(fmt.Sprintf("[后台任务] [%s] 播报结果", r.TaskID))
	h.cancelSpeech()
	h.talkRound.Add(1)
	atomic.StoreInt32(&h.serverVoiceStop, 0)
	if h.dialogueManager != nil {
		h.dialogueManager.Put(chat.Message{Role: "assistant", Content: memory})
	}
	if err := h.sendTTSMessage("start", "", 0); err != nil {
		h.LogError(fmt.Sprintf("[后台任务] 发送TTS开始状态失败: %v", err))
		return
	}
	h.SystemSpeak(speech)
}

func delegateAnnouncement(r delegate.Result) (speech, memory string) {
	switch {
	case r.Err == nil:
		text := strings.TrimSpace(reCodeFence.ReplaceAllString(r.Text, "（代码已省略）"))
		memory = "后台任务「" + truncateRunes(r.Task, 60) + "」的结果：" + truncateRunes(r.Text, delegateMemoryRunes)
		return "你之前交代的任务完成了。" + truncateRunes(text, delegateSpeakMaxRunes), memory
	case errors.Is(r.Err, context.DeadlineExceeded):
		speech = "抱歉，后台任务「" + truncateRunes(r.Task, 30) + "」超时了，没有完成。"
	default:
		speech = "抱歉，后台任务「" + truncateRunes(r.Task, 30) + "」没有完成。" + delegateErrorHint(r.Err)
	}
	return speech, speech
}

func delegateErrorHint(err error) string {
	var be *delegate.BridgeError
	if errors.As(err, &be) && be.Message != "" {
		return "原因是：" + truncateRunes(be.Message, 80)
	}
	return "后台服务暂时不可用。"
}

func truncateRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "……"
}

func (h *ConnectionHandler) mcp_handler_delegate_task(args interface{}) string {
	task := stringArg(args, "task")
	if task == "" {
		h.SystemSpeak("抱歉，我没听清要处理的任务")
		return "提交失败：任务描述为空"
	}
	m, err := h.delegateManager()
	if err != nil {
		h.LogError(fmt.Sprintf("[后台任务] %v", err))
		h.SystemSpeak("抱歉，后台任务服务还没有配置好")
		return "提交失败：后台任务服务未配置"
	}
	id, err := m.Submit(h.delegateKey(), task, stringArg(args, "context"))
	if errors.Is(err, delegate.ErrTooManyTasks) {
		h.SystemSpeak("后台已经有几个任务在跑了，等它们完成再交给我吧")
		return "提交失败：后台任务已达上限"
	}
	if err != nil {
		h.LogError(fmt.Sprintf("[后台任务] 提交失败: %v", err))
		h.SystemSpeak("抱歉，任务没有提交成功")
		return "提交失败"
	}
	h.LogInfo(fmt.Sprintf("[后台任务] [%s] 已提交: %s", id, task))
	h.SystemSpeak("好的，这个任务我交给后台处理了，完成后告诉你")
	return fmt.Sprintf("已提交后台任务 %s，用户已被告知正在处理，结果完成后会自动播报。", id)
}

func (h *ConnectionHandler) mcp_handler_task_status(args interface{}) string {
	m, err := h.delegateManager()
	if err != nil {
		h.SystemSpeak("后台任务服务还没有配置好")
		return "后台任务服务未配置"
	}
	running, recent := m.Status(h.delegateKey())
	text := delegateStatusText(running, recent, time.Now())
	h.SystemSpeak(text)
	return text
}

func delegateStatusText(running []delegate.TaskInfo, recent []delegate.Result, now time.Time) string {
	if len(running) == 0 {
		if len(recent) == 0 {
			return "现在没有后台任务。"
		}
		last := recent[len(recent)-1]
		state := "已完成"
		if last.Canceled {
			state = "已取消"
		} else if last.Err != nil {
			state = "失败了"
		}
		return "现在没有进行中的后台任务，最近一个「" + truncateRunes(last.Task, 30) + "」" + state + "。"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "有%d个后台任务正在处理。", len(running))
	for _, t := range running {
		minutes := int(now.Sub(t.Started).Minutes())
		fmt.Fprintf(&b, "「%s」", truncateRunes(t.Task, 30))
		if minutes > 0 {
			fmt.Fprintf(&b, "已经进行了%d分钟", minutes)
		} else {
			b.WriteString("刚开始")
		}
		if t.Progress != "" {
			b.WriteString("，目前在" + truncateRunes(t.Progress, 30))
		}
		b.WriteString("。")
	}
	return b.String()
}

func (h *ConnectionHandler) mcp_handler_cancel_task(args interface{}) string {
	m, err := h.delegateManager()
	if err != nil {
		h.SystemSpeak("后台任务服务还没有配置好")
		return "后台任务服务未配置"
	}
	n := m.Cancel(h.delegateKey(), stringArg(args, "task_id"))
	text := "没有可以取消的后台任务"
	if n > 0 {
		text = fmt.Sprintf("已取消%d个后台任务", n)
	}
	h.SystemSpeak(text)
	return text
}
