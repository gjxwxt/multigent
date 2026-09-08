package imbridge

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// LiveCardUpdateRequest encapsulates the state needed to render and patch the Root Post Live Card.
type LiveCardUpdateRequest struct {
	WorkspaceID    string
	ProjectID      string
	TaskID         string
	TaskTitle      string
	PipelineID     string
	Initiator      string
	CurrentStepID  string
	CurrentStep    string
	StepStatus     string // "running", "waiting_approval", "completed", "failed"
	StepIndex      int    // 1-based current step index
	TotalSteps     int    // total steps in pipeline
	Assignee       string
	ElapsedSeconds int
	QualitySummary string // e.g. "✓ 单测 142/142 全部通过 | ✓ 静态代码分析 0 风险"
	ConsoleURL     string // link to console
	ForceImmediate bool   // bypass debounce for terminal or gating states
}

// LiveCardDebouncer throttles rapid updates to Mattermost Live Task Cards to prevent rate-limiting (HTTP 429).
type LiveCardDebouncer struct {
	mu       sync.Mutex
	delay    time.Duration
	pending  map[string]*debouncedUpdate
	updateFn func(ctx context.Context, req LiveCardUpdateRequest) error
}

type debouncedUpdate struct {
	timer *time.Timer
	req   LiveCardUpdateRequest
}

// NewLiveCardDebouncer creates a debouncer with the specified throttle delay.
func NewLiveCardDebouncer(delay time.Duration, updateFn func(ctx context.Context, req LiveCardUpdateRequest) error) *LiveCardDebouncer {
	if delay <= 0 {
		delay = 1500 * time.Millisecond
	}
	return &LiveCardDebouncer{
		delay:    delay,
		pending:  make(map[string]*debouncedUpdate),
		updateFn: updateFn,
	}
}

// Schedule queues a Live Card update. If req.ForceImmediate is true, it flushes synchronously.
func (d *LiveCardDebouncer) Schedule(ctx context.Context, req LiveCardUpdateRequest) error {
	if d == nil || d.updateFn == nil {
		return nil
	}

	key := fmt.Sprintf("%s:%s", req.WorkspaceID, req.TaskID)

	d.mu.Lock()
	if req.ForceImmediate {
		// Cancel existing timer if any
		if existing, ok := d.pending[key]; ok {
			if existing.timer != nil {
				existing.timer.Stop()
			}
			delete(d.pending, key)
		}
		d.mu.Unlock()
		return d.updateFn(ctx, req)
	}

	// If already pending, update payload and reset timer
	if existing, ok := d.pending[key]; ok {
		existing.req = req
		if existing.timer != nil {
			existing.timer.Stop()
		}
		existing.timer = time.AfterFunc(d.delay, func() {
			d.flushKey(key)
		})
		d.mu.Unlock()
		return nil
	}

	// New pending entry
	item := &debouncedUpdate{req: req}
	item.timer = time.AfterFunc(d.delay, func() {
		d.flushKey(key)
	})
	d.pending[key] = item
	d.mu.Unlock()
	return nil
}

func (d *LiveCardDebouncer) flushKey(key string) {
	d.mu.Lock()
	item, ok := d.pending[key]
	if !ok {
		d.mu.Unlock()
		return
	}
	delete(d.pending, key)
	d.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = d.updateFn(ctx, item.req)
}

// FormatLiveCardContent renders the standard Ambient Agentic Workspace Live Task Card Markdown.
func FormatLiveCardContent(req LiveCardUpdateRequest) string {
	title := req.TaskTitle
	if title == "" {
		title = req.TaskID
	}
	pipeline := req.PipelineID
	if pipeline == "" {
		pipeline = "standard"
	}
	initiator := req.Initiator
	if initiator == "" {
		initiator = "system"
	}

	// Build progress bar
	progressBar := renderProgressBar(req.StepIndex, req.TotalSteps)

	// Status badge
	var statusBadge string
	switch req.StepStatus {
	case "completed", "success", "done":
		statusBadge = "✅ 已完成 (Completed)"
	case "failed", "error":
		statusBadge = "❌ 异常阻断 (Failed)"
	case "waiting_approval", "in_review", "review":
		statusBadge = "🚨 等待人工审核 (Action Required)"
	default:
		statusBadge = "🚀 执行中 (In Progress)"
	}

	var durStr string
	if req.ElapsedSeconds > 0 {
		durStr = fmt.Sprintf("%dm %02ds", req.ElapsedSeconds/60, req.ElapsedSeconds%60)
	} else {
		durStr = "< 1m"
	}

	stepName := req.CurrentStep
	if stepName == "" {
		stepName = req.CurrentStepID
	}
	if stepName == "" {
		stepName = "初始化"
	}

	assigneeStr := req.Assignee
	if assigneeStr == "" {
		assigneeStr = "AI Agent"
	}

	content := fmt.Sprintf("### 🚀 [Task] %s\n", title)
	content += fmt.Sprintf("> **Task ID**: `%s` | **Project**: `%s` | **Pipeline**: `%s`\n", req.TaskID, req.ProjectID, pipeline)
	content += fmt.Sprintf("> **Initiator**: `@%s` | **Status**: %s\n\n", initiator, statusBadge)
	content += fmt.Sprintf("**实时进度**: `%s` (%d/%d 步骤)\n", progressBar, req.StepIndex, req.TotalSteps)
	content += fmt.Sprintf("- **当前节点**: **%s** (`%s`) · 负责: `@%s`\n", stepName, req.CurrentStepID, assigneeStr)
	content += fmt.Sprintf("- **累计耗时**: %s\n", durStr)

	if req.QualitySummary != "" {
		content += fmt.Sprintf("- **质量指标**: %s\n", req.QualitySummary)
	}

	content += "\n---\n*💡 本卡片为实时任务看板（原地自动刷新），关键决策与里程碑将在本 Thread 持续沉淀。*"
	if req.ConsoleURL != "" {
		content += fmt.Sprintf(" [🔗 在 Multigent 控制台查看详情](%s)", req.ConsoleURL)
	}
	return content
}

func renderProgressBar(current, total int) string {
	if total <= 0 {
		total = 10
	}
	if current < 0 {
		current = 0
	}
	if current > total {
		current = total
	}
	barLength := 10
	filled := (current * barLength) / total
	if filled > barLength {
		filled = barLength
	}

	var bar string
	for i := 0; i < filled; i++ {
		bar += "█"
	}
	for i := filled; i < barLength; i++ {
		bar += "░"
	}
	pct := (current * 100) / total
	return fmt.Sprintf("%s %d%%", bar, pct)
}
