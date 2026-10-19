package delegate

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	maxPendingResults = 5
	maxRecentResults  = 5
	pendingTTL        = 24 * time.Hour
)

// ErrTooManyTasks is returned when a device already runs MaxConcurrent tasks.
var ErrTooManyTasks = errors.New("too many delegate tasks")

// Runner executes one task; *Client implements it.
type Runner interface {
	Run(ctx context.Context, task Task, onProgress func(string)) (string, error)
}

// ResultSource is implemented by runners that keep finished tasks themselves
// (*Client: the bridge stores them until acknowledged). The manager fetches
// them when a device attaches and acknowledges them once spoken.
type ResultSource interface {
	Fetch(ctx context.Context, deviceID string) ([]StoredResult, error)
	Ack(ctx context.Context, deviceID string, taskIDs []string) error
}

// Result is a finished task. Err is nil on success.
type Result struct {
	TaskID   string
	Task     string
	Text     string
	Err      error
	Canceled bool
	Finished time.Time
}

// TaskInfo describes a running task.
type TaskInfo struct {
	ID       string
	Task     string
	Started  time.Time
	Progress string
}

type runningTask struct {
	info     TaskInfo
	cancel   context.CancelFunc
	canceled bool
}

type deviceState struct {
	running   map[string]*runningTask
	pending   []Result             // finished but not yet spoken
	recent    []Result             // last finished tasks, for status queries
	delivered map[string]time.Time // spoken ids, so a fetch racing an ack does not repeat them
	notify    func()
	notifyID  uint64
}

// Manager runs delegated tasks per device. Tasks outlive the WebSocket
// connection that started them: devices disconnect when idle, so results are
// kept until the device reconnects and takes them.
type Manager struct {
	runner        Runner
	timeout       time.Duration
	maxConcurrent int

	ctx    context.Context
	stop   context.CancelCauseFunc
	log    func(string)
	mu     sync.Mutex
	nextID uint64
	devs   map[string]*deviceState
	now    func() time.Time
}

// NewManager creates a manager. timeout bounds each task; maxConcurrent
// bounds running tasks per device.
func NewManager(runner Runner, timeout time.Duration, maxConcurrent int) *Manager {
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	ctx, stop := context.WithCancelCause(context.Background())
	return &Manager{runner: runner, timeout: timeout, maxConcurrent: maxConcurrent, ctx: ctx, stop: stop, log: func(string) {}, devs: map[string]*deviceState{}, now: time.Now}
}

// SetLogger sets where background fetch/ack failures are reported.
func (m *Manager) SetLogger(log func(string)) {
	if log != nil {
		m.log = log
	}
}

func (m *Manager) device(id string) *deviceState {
	d := m.devs[id]
	if d == nil {
		d = &deviceState{running: map[string]*runningTask{}, delivered: map[string]time.Time{}}
		m.devs[id] = d
	}
	return d
}

// Submit starts a task in the background and returns its id.
func (m *Manager) Submit(deviceID, text, taskContext string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("delegate task text is empty")
	}
	m.mu.Lock()
	if m.ctx.Err() != nil {
		m.mu.Unlock()
		return "", errors.New("delegate manager is closed")
	}
	d := m.device(deviceID)
	if len(d.running) >= m.maxConcurrent {
		m.mu.Unlock()
		return "", ErrTooManyTasks
	}
	id := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	var ctx context.Context
	var cancel context.CancelFunc
	if m.timeout > 0 {
		ctx, cancel = context.WithTimeout(m.ctx, m.timeout)
	} else {
		ctx, cancel = context.WithCancel(m.ctx)
	}
	rt := &runningTask{info: TaskInfo{ID: id, Task: text, Started: m.now()}, cancel: cancel}
	d.running[id] = rt
	m.mu.Unlock()

	go m.run(ctx, deviceID, rt, Task{ID: id, DeviceID: deviceID, Text: text, Context: strings.TrimSpace(taskContext)})
	return id, nil
}

func (m *Manager) run(ctx context.Context, deviceID string, rt *runningTask, task Task) {
	defer rt.cancel()
	text, err := m.runner.Run(ctx, task, func(progress string) {
		m.mu.Lock()
		rt.info.Progress = progress
		m.mu.Unlock()
	})
	if err == nil && strings.TrimSpace(text) == "" {
		err = errors.New("delegate bridge returned an empty result")
	}
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = context.DeadlineExceeded
	}

	m.mu.Lock()
	d := m.device(deviceID)
	delete(d.running, task.ID)
	result := Result{TaskID: task.ID, Task: task.Text, Text: strings.TrimSpace(text), Err: err, Canceled: rt.canceled, Finished: m.now()}
	d.recent = appendCapped(d.recent, result, maxRecentResults)
	var notify func()
	// A canceled task was requested by the user; do not announce it again.
	// Shutdown also cancels, and there is nobody to speak to then.
	if !rt.canceled && m.ctx.Err() == nil {
		d.pending = appendCapped(d.pending, result, maxPendingResults)
		notify = d.notify
	}
	m.mu.Unlock()
	if notify != nil {
		notify()
	}
}

func appendCapped(list []Result, r Result, limit int) []Result {
	list = append(list, r)
	if len(list) > limit {
		list = append([]Result(nil), list[len(list)-limit:]...)
	}
	return list
}

// Status returns running tasks and recently finished ones, oldest first.
func (m *Manager) Status(deviceID string) ([]TaskInfo, []Result) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.devs[deviceID]
	if d == nil {
		return nil, nil
	}
	running := make([]TaskInfo, 0, len(d.running))
	for _, rt := range d.running {
		running = append(running, rt.info)
	}
	for i := 1; i < len(running); i++ { // few entries; insertion sort by start
		for j := i; j > 0 && running[j].Started.Before(running[j-1].Started); j-- {
			running[j], running[j-1] = running[j-1], running[j]
		}
	}
	return running, append([]Result(nil), d.recent...)
}

// Cancel stops one task, or all running tasks of the device when taskID is
// empty. It returns the number of tasks canceled.
func (m *Manager) Cancel(deviceID, taskID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.devs[deviceID]
	if d == nil {
		return 0
	}
	n := 0
	for id, rt := range d.running {
		if taskID == "" || id == taskID {
			if !rt.canceled {
				rt.canceled = true
				rt.cancel()
				n++
			}
		}
	}
	return n
}

// Attach registers the live connection of a device. notify is called (from
// any goroutine) whenever a new result is pending. Only the latest
// attachment is kept; detach is a no-op once another connection attached.
func (m *Manager) Attach(deviceID string, notify func()) (detach func()) {
	m.mu.Lock()
	m.nextID++
	id := m.nextID
	d := m.device(deviceID)
	d.notify, d.notifyID = notify, id
	hasPending := len(d.pending) > 0
	m.mu.Unlock()
	if hasPending && notify != nil {
		notify()
	}
	if src, ok := m.runner.(ResultSource); ok {
		go m.syncFromBridge(src, deviceID)
	}
	return func() {
		m.mu.Lock()
		if d := m.devs[deviceID]; d != nil && d.notifyID == id {
			d.notify = nil
		}
		m.mu.Unlock()
	}
}

// syncFromBridge picks up results the bridge stored while this process was
// not waiting for them (xiaozhi-server restarted, or the task connection
// dropped). Results still running here, already queued, or already spoken are
// skipped.
func (m *Manager) syncFromBridge(src ResultSource, deviceID string) {
	stored, err := src.Fetch(m.ctx, deviceID)
	if err != nil {
		if m.ctx.Err() == nil {
			m.log("fetch delegate results: " + err.Error())
		}
		return
	}
	m.mu.Lock()
	d := m.device(deviceID)
	added := false
	for _, sr := range stored {
		if sr.Code == "canceled" || m.now().Sub(sr.FinishedAt) > pendingTTL {
			continue
		}
		if _, ok := d.running[sr.TaskID]; ok {
			continue
		}
		if _, ok := d.delivered[sr.TaskID]; ok {
			continue
		}
		if containsResult(d.pending, sr.TaskID) {
			continue
		}
		r := Result{TaskID: sr.TaskID, Task: sr.Task, Text: strings.TrimSpace(sr.Text), Finished: sr.FinishedAt}
		if sr.Code != "" {
			r.Text = ""
			r.Err = &BridgeError{Code: sr.Code, Message: sr.Message}
		}
		d.pending = appendCapped(d.pending, r, maxPendingResults)
		added = true
	}
	notify := d.notify
	m.mu.Unlock()
	if added && notify != nil {
		notify()
	}
}

func containsResult(list []Result, taskID string) bool {
	for _, r := range list {
		if r.TaskID == taskID {
			return true
		}
	}
	return false
}

// Delivered records that a result was spoken and tells the bridge to drop it.
func (m *Manager) Delivered(deviceID, taskID string) {
	m.mu.Lock()
	d := m.device(deviceID)
	now := m.now()
	d.delivered[taskID] = now
	for id, at := range d.delivered {
		if now.Sub(at) > pendingTTL {
			delete(d.delivered, id)
		}
	}
	m.mu.Unlock()
	src, ok := m.runner.(ResultSource)
	if !ok {
		return
	}
	go func() {
		if err := src.Ack(m.ctx, deviceID, []string{taskID}); err != nil && m.ctx.Err() == nil {
			m.log("ack delegate result " + taskID + ": " + err.Error())
		}
	}()
}

// TakePending pops the oldest undelivered result that has not expired.
func (m *Manager) TakePending(deviceID string) (Result, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.devs[deviceID]
	if d == nil {
		return Result{}, false
	}
	for len(d.pending) > 0 {
		r := d.pending[0]
		d.pending = d.pending[1:]
		if m.now().Sub(r.Finished) <= pendingTTL {
			return r, true
		}
	}
	return Result{}, false
}

// HasPending reports whether a result is waiting to be spoken.
func (m *Manager) HasPending(deviceID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.devs[deviceID]
	return d != nil && len(d.pending) > 0
}

// Close stops waiting for running tasks. The bridge keeps running them and
// stores the results, which the next process fetches on Attach.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stop(ErrShutdown)
}
