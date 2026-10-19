package delegate

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeRunner struct {
	release chan struct{}
	text    string
	err     error
}

func (f *fakeRunner) Run(ctx context.Context, task Task, onProgress func(string)) (string, error) {
	onProgress("working")
	select {
	case <-f.release:
		return f.text, f.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func waitSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}
}

func TestManagerDeliversResultToAttachedDevice(t *testing.T) {
	r := &fakeRunner{release: make(chan struct{}), text: "答案"}
	m := NewManager(r, time.Minute, 1)
	defer m.Close()
	notified := make(chan struct{}, 4)
	detach := m.Attach("dev", func() { notified <- struct{}{} })
	defer detach()

	id, err := m.Submit("dev", "查资料", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Submit("dev", "第二个", ""); !errors.Is(err, ErrTooManyTasks) {
		t.Fatalf("want ErrTooManyTasks, got %v", err)
	}
	if _, err := m.Submit("other", "别的设备", ""); err != nil {
		t.Fatalf("other device must not share the limit: %v", err)
	}
	running, _ := m.Status("dev")
	if len(running) != 1 || running[0].ID != id {
		t.Fatalf("running = %+v", running)
	}
	close(r.release)
	waitSignal(t, notified)
	res, ok := m.TakePending("dev")
	if !ok || res.TaskID != id || res.Text != "答案" || res.Err != nil {
		t.Fatalf("result = %+v, %v", res, ok)
	}
	if _, ok := m.TakePending("dev"); ok {
		t.Fatal("result delivered twice")
	}
}

func TestManagerKeepsResultForReconnect(t *testing.T) {
	r := &fakeRunner{release: make(chan struct{}), text: "完成"}
	m := NewManager(r, time.Minute, 2)
	defer m.Close()
	detach := m.Attach("dev", func() {})
	if _, err := m.Submit("dev", "任务", ""); err != nil {
		t.Fatal(err)
	}
	detach() // device went offline
	close(r.release)
	deadline := time.Now().Add(2 * time.Second)
	for !m.HasPending("dev") {
		if time.Now().After(deadline) {
			t.Fatal("result was not stored")
		}
		time.Sleep(10 * time.Millisecond)
	}
	notified := make(chan struct{}, 1)
	m.Attach("dev", func() { notified <- struct{}{} })
	waitSignal(t, notified)
}

func TestManagerCancelAndTimeout(t *testing.T) {
	r := &fakeRunner{release: make(chan struct{})}
	m := NewManager(r, 50*time.Millisecond, 2)
	defer m.Close()
	notified := make(chan struct{}, 4)
	m.Attach("dev", func() { notified <- struct{}{} })

	if _, err := m.Submit("dev", "会取消", ""); err != nil {
		t.Fatal(err)
	}
	if n := m.Cancel("dev", ""); n != 1 {
		t.Fatalf("canceled %d", n)
	}
	if _, err := m.Submit("dev", "会超时", ""); err != nil {
		t.Fatal(err)
	}
	waitSignal(t, notified)
	res, ok := m.TakePending("dev")
	if !ok || !errors.Is(res.Err, context.DeadlineExceeded) || res.Task != "会超时" {
		t.Fatalf("want timeout result, got %+v", res)
	}
	if _, ok := m.TakePending("dev"); ok {
		t.Fatal("canceled task must not be announced")
	}
	_, recent := m.Status("dev")
	if len(recent) != 2 {
		t.Fatalf("recent = %+v", recent)
	}
}

type sourceRunner struct {
	fakeRunner
	mu     sync.Mutex
	stored []StoredResult
	acked  []string
}

func (s *sourceRunner) Fetch(ctx context.Context, deviceID string) ([]StoredResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]StoredResult(nil), s.stored...), nil
}

func (s *sourceRunner) Ack(ctx context.Context, deviceID string, ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acked = append(s.acked, ids...)
	return nil
}

func TestManagerFetchesStoredResultsOnAttach(t *testing.T) {
	now := time.Now()
	r := &sourceRunner{stored: []StoredResult{
		{TaskID: "a", Task: "旧任务", Text: "旧答案", FinishedAt: now},
		{TaskID: "b", Task: "失败", Code: "agent_error", Message: "agent failed", FinishedAt: now},
		{TaskID: "c", Task: "取消", Code: "canceled", FinishedAt: now},
		{TaskID: "d", Task: "过期", Text: "x", FinishedAt: now.Add(-48 * time.Hour)},
	}}
	m := NewManager(r, time.Minute, 1)
	defer m.Close()
	notified := make(chan struct{}, 4)
	m.Attach("dev", func() { notified <- struct{}{} })
	waitSignal(t, notified)

	first, ok := m.TakePending("dev")
	if !ok || first.TaskID != "a" || first.Text != "旧答案" || first.Err != nil {
		t.Fatalf("first = %+v", first)
	}
	m.Delivered("dev", first.TaskID)
	second, ok := m.TakePending("dev")
	var be *BridgeError
	if !ok || second.TaskID != "b" || !errors.As(second.Err, &be) || be.Message != "agent failed" {
		t.Fatalf("second = %+v", second)
	}
	if _, ok := m.TakePending("dev"); ok {
		t.Fatal("canceled and expired results must be skipped")
	}

	// A reconnect before the bridge processed the ack must not repeat "a".
	m.Attach("dev", func() { notified <- struct{}{} })
	deadline := time.Now().Add(2 * time.Second)
	for {
		r.mu.Lock()
		n := len(r.acked)
		r.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("delivered result was not acked")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if res, ok := m.TakePending("dev"); ok && res.TaskID == "a" {
		t.Fatal("delivered result announced twice")
	}
}
