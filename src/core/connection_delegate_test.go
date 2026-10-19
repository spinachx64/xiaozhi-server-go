package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"xiaozhi-server-go/src/core/delegate"
)

type stubRunner struct {
	text string
	err  error
}

func (r stubRunner) Run(ctx context.Context, task delegate.Task, onProgress func(string)) (string, error) {
	onProgress("搜索资料")
	return r.text, r.err
}

func delegateHandler(t *testing.T, runner delegate.Runner) *ConnectionHandler {
	t.Helper()
	h := realtimeHandler(t, "pcm")
	h.ttsQueue = make(chan struct {
		text      string
		round     int
		textIndex int
	}, 20)
	h.tts_last_text_index.Store(-1)
	h.deviceID = "aa:bb"
	h.delegates = delegate.NewManager(runner, time.Minute, 2)
	t.Cleanup(h.delegates.Close)
	return h
}

func TestDelegateResultIsAnnouncedWhenIdle(t *testing.T) {
	h := delegateHandler(t, stubRunner{text: "北京明天晴，最高二十度。"})
	detach := h.startDelegateDelivery()
	if detach == nil {
		t.Fatal("delivery not started")
	}
	defer detach()
	before := h.currentRound()
	if _, err := h.delegates.Submit(h.delegateKey(), "查北京天气", ""); err != nil {
		t.Fatal(err)
	}
	var spoken []string
	deadline := time.After(5 * time.Second)
	for len(spoken) == 0 || !strings.Contains(strings.Join(spoken, ""), "二十度") {
		select {
		case task := <-h.ttsQueue:
			spoken = append(spoken, task.text)
		case <-deadline:
			t.Fatalf("result not spoken: %v", spoken)
		}
	}
	if !strings.Contains(spoken[0], "任务完成了") {
		t.Fatalf("missing announcement prefix: %v", spoken)
	}
	if h.currentRound() != before+1 {
		t.Fatalf("announcement should start a new round, got %d want %d", h.currentRound(), before+1)
	}
}

func TestDelegateWaitsWhileGenerating(t *testing.T) {
	h := delegateHandler(t, stubRunner{text: "完成。"})
	h.beginSpeechGeneration(h.currentRound())
	if h.deviceIdle() {
		t.Fatal("device generating a reply must not be idle")
	}
	h.endSpeechGeneration(h.currentRound())
	if !h.deviceIdle() {
		t.Fatal("device should be idle after generation ends")
	}
	h.tts_last_text_index.Store(2)
	if h.deviceIdle() {
		t.Fatal("device with unfinished playback must not be idle")
	}
}

func TestDelegateAnnouncement(t *testing.T) {
	speech, memory := delegateAnnouncement(delegate.Result{Task: "写脚本", Text: "做好了\n```sh\necho hi\n```"})
	if strings.Contains(speech, "echo") || !strings.Contains(speech, "代码已省略") {
		t.Fatalf("code should not be spoken: %q", speech)
	}
	if !strings.Contains(memory, "echo hi") {
		t.Fatalf("history should keep full result: %q", memory)
	}
	speech, _ = delegateAnnouncement(delegate.Result{Task: "查资料", Err: context.DeadlineExceeded})
	if !strings.Contains(speech, "超时") {
		t.Fatal(speech)
	}
	speech, _ = delegateAnnouncement(delegate.Result{Task: "查资料", Err: &delegate.BridgeError{Code: "busy", Message: "模型不可用"}})
	if !strings.Contains(speech, "模型不可用") {
		t.Fatal(speech)
	}
	speech, _ = delegateAnnouncement(delegate.Result{Task: "查资料", Err: errors.New("dial tcp: refused")})
	if strings.Contains(speech, "dial") {
		t.Fatalf("internal errors must not be spoken: %q", speech)
	}
}

func TestDelegateStatusText(t *testing.T) {
	now := time.Now()
	if got := delegateStatusText(nil, nil, now); got != "现在没有后台任务。" {
		t.Fatal(got)
	}
	got := delegateStatusText([]delegate.TaskInfo{{Task: "整理文件", Started: now.Add(-3 * time.Minute), Progress: "读取目录"}}, nil, now)
	if !strings.Contains(got, "3分钟") || !strings.Contains(got, "读取目录") {
		t.Fatal(got)
	}
	got = delegateStatusText(nil, []delegate.Result{{Task: "查天气", Canceled: true}}, now)
	if !strings.Contains(got, "已取消") {
		t.Fatal(got)
	}
}

func TestDelegateHandlersSubmitAndCancel(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	h := delegateHandler(t, blockingRunner(block))
	if got := h.mcp_handler_delegate_task(map[string]interface{}{"task": "  "}); !strings.Contains(got, "为空") {
		t.Fatal(got)
	}
	if got := h.mcp_handler_delegate_task(map[string]interface{}{"task": "整理下载目录"}); !strings.Contains(got, "已提交") {
		t.Fatal(got)
	}
	if got := h.mcp_handler_cancel_task(map[string]interface{}{}); got != "已取消1个后台任务" {
		t.Fatal(got)
	}
}

type blockingRunner chan struct{}

func (b blockingRunner) Run(ctx context.Context, task delegate.Task, onProgress func(string)) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-b:
		return "done", nil
	}
}
