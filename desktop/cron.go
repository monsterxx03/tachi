package main

// Desktop scheduled tasks: the in-process cron scheduler, the bindings its
// panel uses, and the trigger that runs one job into a conversation.
//
// Two facts shape all of it:
//
//   - A job belongs to ONE conversation, identified by
//     session.Session.LogicalConversationID. Compaction moves the conversation
//     into a successor session but keeps that id, so a job never has to be
//     re-bound. The job's target is the leaf of the chain (compacted_child_id
//     == ""), resolved fresh on every run.
//   - A job belongs to ONE entry point. channel and desktop share crons.json;
//     each scheduler is constructed with an Owns filter, so a job is only ever
//     fired by the process that can actually deliver it.
//
// Design: docs/2026-10-18-desktop-cron-design.md

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/atfile"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/cron"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/pkg/strutil"
	"github.com/monsterxx03/tachi/session"
)

const (
	cronTurnEventName        = "agent:cron_turn"
	cronChangedEventName     = "agent:cron_changed"
	sessionsChangedEventName = "agent:sessions_changed"

	cronPreviewCount = 5
	// cronDisplayPromptMaxRune bounds the prompt snippet in the transcript
	// bubble. The model still gets the whole prompt; this is the label.
	cronDisplayPromptMaxRune = 120
)

// cronUnattendedPrompt is appended to every scheduled run's prompt. It is
// stable (no timestamp), so the job's prefix is byte-identical on every run and
// the conversation's prompt cache survives.
const cronUnattendedPrompt = "[Scheduled run]\n" +
	"This turn was started by a scheduled job while the user may be away. Do not ask the user questions; " +
	"if something needs a decision, state it in your reply."

// CronTurnEvent tells the frontend a scheduled turn just started, so it can put
// the task's user bubble into the conversation (the backend never types into a
// turn the way a person does).
type CronTurnEvent struct {
	SessionID string `json:"sessionId"`
	JobID     string `json:"jobId"`
	JobName   string `json:"jobName"`
	Schedule  string `json:"schedule"`
	Display   string `json:"display"`
}

// CronJobVO is one desktop job as the panel and the editor see it.
type CronJobVO struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Schedule     string `json:"schedule"`
	ScheduleText string `json:"scheduleText"`
	Type         string `json:"type"`
	Prompt       string `json:"prompt"`
	Status       string `json:"status"`
	Notify       string `json:"notify"`
	Timezone     string `json:"timezone"`

	// TargetKind is "session" (bound), "pending" (a dedicated conversation is
	// created on the first run) or "missing" (its conversation was deleted).
	TargetKind         string `json:"targetKind"`
	TargetTitle        string `json:"targetTitle,omitempty"`
	TargetSessionID    string `json:"targetSessionId,omitempty"` // logical id
	TargetIsCurrent    bool   `json:"targetIsCurrent,omitempty"`
	TargetProjectID    string `json:"targetProjectId,omitempty"`
	TargetWorkingDir   string `json:"targetWorkingDir,omitempty"`
	TargetProvider     string `json:"targetProvider,omitempty"`
	TargetSkipApproval bool   `json:"targetSkipApproval,omitempty"`

	NextRun      time.Time `json:"nextRun,omitempty"`
	NextRunError string    `json:"nextRunError,omitempty"`
	LastRunAt    time.Time `json:"lastRunAt,omitempty"`
	LastRunState string    `json:"lastRunStatus,omitempty"`
	LastRunError string    `json:"lastRunError,omitempty"`

	Running   bool      `json:"running,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// CronJobInput is the editor's save payload. It is deliberately not cron.Job:
// the frontend never writes ids, timestamps or last-run state.
type CronJobInput struct {
	ID       string `json:"id"` // "" = create
	Name     string `json:"name"`
	Schedule string `json:"schedule"`
	Prompt   string `json:"prompt"`
	Type     string `json:"type"`
	Notify   string `json:"notify"`
	Timezone string `json:"timezone"`

	// Mode is "session" (bind to SessionID) or "new" (create a dedicated
	// conversation on the first run, from the template fields below).
	Mode           string   `json:"mode"`
	SessionID      string   `json:"sessionId"`
	ProjectID      string   `json:"projectId"`
	WorkingDir     string   `json:"workingDir"`
	AdditionalDirs []string `json:"additionalDirs"`
	ProviderName   string   `json:"providerName"`
	SkipApproval   bool     `json:"skipApproval"`
}

// CronPreviewVO is the editor's live schedule preview.
type CronPreviewVO struct {
	Next          []time.Time `json:"next"`
	Error         string      `json:"error,omitempty"`
	LocalTimezone string      `json:"localTimezone"`
}

// CronTargetVO is one conversation the editor can bind a job to. A compaction
// chain yields ONE entry (its leaf), so the user cannot pick a page that is no
// longer the conversation.
type CronTargetVO struct {
	SessionID    string    `json:"sessionId"` // logical conversation id
	Title        string    `json:"title"`
	ProjectID    string    `json:"projectId,omitempty"`
	ProjectName  string    `json:"projectName,omitempty"`
	WorkingDir   string    `json:"workingDir,omitempty"`
	ProviderName string    `json:"providerName,omitempty"`
	SkipApproval bool      `json:"skipApproval,omitempty"`
	IsCurrent    bool      `json:"isCurrent,omitempty"`
	UpdatedAt    time.Time `json:"updatedAt,omitempty"`
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

// startCron builds and starts the desktop scheduler. Cron disabled (or no
// config at all) leaves d.cron nil, which the panel reports rather than
// pretending a task will run.
func (d *desktopApp) startCron(ctx context.Context) {
	// The cleanup hint must be in place even when the scheduler is disabled:
	// the jobs still exist, and a session cleanup in another entry point would
	// otherwise be free to delete their conversations.
	d.syncCronProtection()
	cfg := d.config()
	if cfg == nil || !cfg.Cron.IsEnabled() {
		return
	}
	s := cron.NewScheduler(cron.SchedulerConfig{
		Store:            cron.NewStore(d.cronStorePath()),
		Handler:          d.onCronTrigger,
		Logger:           logger.New("desktop"),
		MaxConcurrent:    cfg.Cron.MaxConcurrent,
		ExecutionTimeout: cfg.Cron.ExecutionTimeout,
		// The desktop only fires jobs it can deliver: a channel job has a
		// thread, not a conversation. Empty target type is an old channel job.
		Owns: func(j *cron.Job) bool {
			return j.TargetType == cron.TargetTypeDesktop
		},
		// A finished run's LastRun is written AFTER the handler returns, so the
		// task panel needs this hook rather than the handler's own defer.
		OnFinished: func(j *cron.Job) {
			d.emitCronChanged(j.ID)
			// A oneshot has just removed itself; its conversation must stop
			// being exempt from cleanup.
			d.syncCronProtection()
		},
	})
	if err := s.Start(ctx); err != nil {
		logger.New("desktop").Error(ctx, "cron: scheduler unavailable", err)
		return
	}
	d.cronMu.Lock()
	d.cron = s
	d.cronMu.Unlock()
}

func (d *desktopApp) stopCron() {
	d.cronMu.Lock()
	s := d.cron
	d.cron = nil
	d.cronMu.Unlock()
	if s != nil {
		s.Stop()
	}
}

func (d *desktopApp) cronScheduler() *cron.Scheduler {
	d.cronMu.Lock()
	defer d.cronMu.Unlock()
	return d.cron
}

func (d *desktopApp) cronStorePath() string {
	if cfg := d.config(); cfg != nil && strings.TrimSpace(cfg.Cron.StorePath) != "" {
		return cfg.Cron.StorePath
	}
	return cron.DefaultStorePath()
}

// cronJobRecords reads every job, through the scheduler when it exists and
// straight from the store when cron is disabled (the panel stays readable).
func (d *desktopApp) cronJobRecords() ([]*cron.Job, error) {
	if s := d.cronScheduler(); s != nil {
		return s.List()
	}
	return cron.NewStore(d.cronStorePath()).List()
}

func (d *desktopApp) markCronRunning(id string, running bool) {
	d.mu.Lock()
	if running {
		d.cronRunning[id] = true
	} else {
		delete(d.cronRunning, id)
	}
	d.mu.Unlock()
	d.emitCronChanged(id)
}

// emitCronChanged tells every screen the job table moved — the window
// (agent:cron_changed) and every phone (cron_changed). A job created, edited, paused, deleted or
// finished is one fact with one exit: the phone's panel rereads the table from it, so a change
// made on either screen is visible on the other without a reconnect (issue: 双端同步).
func (d *desktopApp) emitCronChanged(id string) {
	if d.app != nil {
		d.app.Event.Emit(cronChangedEventName, id)
	}
	d.mobileBroadcast(map[string]any{"type": mobileEvCronChanged, "id": id})
}

// emitSessionsChanged tells EVERY screen the conversation list moved — the window
// (agent:sessions_changed, so the sidebar re-reads it) and every phone (sessions_changed).
// id is the conversation the change is about, for a window that wants to look at just that one.
//
// One function for one fact: a rename, a pin, a create and a delete all move the list, and the
// phone has no other way to hear about a change made on the machine (issue #57).
func (d *desktopApp) emitSessionsChanged(id string) {
	if d.app != nil {
		d.app.Event.Emit(sessionsChangedEventName, id)
	}
	d.mobileBroadcast(map[string]any{"type": mobileEvSessionsChanged})
}

// ---------------------------------------------------------------------------
// Resolution: which conversation does this job write into?
// ---------------------------------------------------------------------------

// leafSession resolves a logical conversation id to the session the
// conversation currently lives in: the leaf of the compaction chain. When a
// stale tree somehow produces more than one leaf, the newest one wins.
func (d *desktopApp) leafSession(logicalID string) (*session.Session, error) {
	if logicalID == "" {
		return nil, fmt.Errorf("定时任务没有目标会话")
	}
	if d.sm == nil {
		return nil, fmt.Errorf("会话存储不可用")
	}
	list, err := d.sm.List()
	if err != nil {
		return nil, fmt.Errorf("读取会话列表失败：%w", err)
	}
	var best *session.Session
	for _, s := range list {
		if s.CompactedChildID != "" || s.LogicalConversationID() != logicalID {
			continue
		}
		if best == nil || s.UpdatedAt.After(best.UpdatedAt) {
			best = s
		}
	}
	if best == nil {
		return nil, fmt.Errorf("目标会话已不存在")
	}
	return best, nil
}

// leafSessionsByLogical is the one scan ListCronJobs and the sidebar's cron
// count share: logical id → the session the conversation lives in now.
func (d *desktopApp) leafSessionsByLogical() map[string]*session.Session {
	out := make(map[string]*session.Session)
	if d.sm == nil {
		return out
	}
	list, err := d.sm.List()
	if err != nil {
		return out
	}
	for _, s := range list {
		if s.CompactedChildID != "" {
			continue
		}
		id := s.LogicalConversationID()
		if prev := out[id]; prev == nil || s.UpdatedAt.After(prev.UpdatedAt) {
			out[id] = s
		}
	}
	return out
}

// cronCountsByLogical is how many desktop jobs write into each conversation.
// It is what the sidebar's badge and the composer's clock count are drawn
// from: one scan for the whole list, and the count is a property of the
// conversation (its logical id), not of one page of a compaction chain.
func (d *desktopApp) cronCountsByLogical() map[string]int {
	out := make(map[string]int)
	jobs, err := d.cronJobRecords()
	if err != nil {
		return out
	}
	for _, j := range jobs {
		if j.TargetType != cron.TargetTypeDesktop || j.Desktop == nil {
			continue
		}
		if j.Desktop.SessionID != "" {
			out[j.Desktop.SessionID]++
		}
	}
	return out
}

// syncCronProtection stamps Session.CronBound on the conversations a desktop
// job writes into (and clears it everywhere else).
//
// Cleanup is count-based and runs inside EVERY entry point — the desktop, the
// TUI, ACP, a channel process — so it cannot consult crons.json: only the
// session record is common. This flag is the persisted bridge, maintained here
// on job create/update/delete, at startup, and after a run (a oneshot deletes
// itself). Compaction moves it to the child, because the child is the row a
// cleanup would consider.
func (d *desktopApp) syncCronProtection() {
	if d.sm == nil {
		return
	}
	jobs, err := d.cronJobRecords()
	if err != nil {
		// An unreadable store must not clear every flag: the sessions would then
		// lose the only protection the sweep can see. Leave them as they are.
		return
	}
	targets := make(map[string]bool)
	for _, j := range jobs {
		if j.TargetType != cron.TargetTypeDesktop || j.Desktop == nil {
			continue
		}
		if j.Desktop.SessionID != "" {
			targets[j.Desktop.SessionID] = true
		}
	}
	sessions, err := d.sm.List()
	if err != nil {
		return
	}
	for _, s := range sessions {
		// Only the LEAF can be the target; a parent is history and must not keep
		// a flag (a stale one would exempt a hidden ancestor forever). So a
		// non-leaf's wanted value is always false.
		want := false
		if s.CompactedChildID == "" {
			want = targets[s.LogicalConversationID()]
		}
		if s.CronBound == want {
			continue
		}
		s.CronBound = want
		if err := d.sm.UpdateMeta(s); err != nil {
			logger.New("desktop").Warn(context.Background(),
				"cron: could not write the cleanup-protection flag", "session", s.ID, "want", want, "err", err)
		}
	}
}

// resolveCronTarget returns the session this run writes into, creating and
// binding a dedicated conversation the first time a "new" job fires.
func (d *desktopApp) resolveCronTarget(ctx context.Context, job *cron.Job) (string, error) {
	if job.Desktop.SessionID != "" {
		leaf, err := d.leafSession(job.Desktop.SessionID)
		if err != nil {
			d.pauseCronJob(ctx, job, err)
			return "", err
		}
		return leaf.ID, nil
	}
	if job.Desktop.Mode != cron.DesktopModeNew {
		err := fmt.Errorf("定时任务没有可用的目标会话")
		d.pauseCronJob(ctx, job, err)
		return "", err
	}

	sess, err := d.createCronSession(job)
	if err != nil {
		return "", err
	}
	sched := d.cronScheduler()
	if sched == nil {
		_ = d.sm.Delete(sess.ID)
		return "", fmt.Errorf("调度器不可用")
	}
	updated, err := sched.BindDesktopSession(job.ID, sess.LogicalConversationID())
	if err != nil {
		// Never leave a conversation the task does not know about: this run
		// did not happen, and the next one starts from a clean slate.
		_ = d.sm.Delete(sess.ID)
		return "", fmt.Errorf("绑定专属会话失败：%w", err)
	}
	// The handler's copy drives the rest of THIS run (the wait loop re-resolves
	// its leaf, and the tool target names its conversation). The store is now
	// bound; keep the in-memory copy in step instead of leaving Mode=new.
	if updated != nil {
		job.Desktop = updated.Desktop
	}
	d.emitCronChanged(job.ID)
	return sess.ID, nil
}

// pauseCronJob stops a job whose target is gone. The scheduler's own LastRun
// update runs after this handler returns, so the reason lands on the record.
func (d *desktopApp) pauseCronJob(ctx context.Context, job *cron.Job, cause error) {
	sched := d.cronScheduler()
	if sched == nil {
		return
	}
	if _, err := sched.Pause(job.ID); err != nil {
		logger.New("desktop").Error(ctx, "cron: could not pause a job with no target", err, "id", job.ID)
		return
	}
	logger.New("desktop").Warn(ctx, "cron: target conversation is gone; job paused", "id", job.ID, "reason", cause.Error())
	d.emitCronChanged(job.ID)
}

// createCronSession builds the dedicated conversation a "new" job runs in.
// The session's own record carries the approval mode, so every later run
// reads it from there.
func (d *desktopApp) createCronSession(job *cron.Job) (*session.Session, error) {
	t := job.Desktop
	sm := d.newSessionManager()
	if sm == nil {
		return nil, fmt.Errorf("无法创建会话管理器")
	}

	provider := strings.TrimSpace(t.ProviderName)
	if provider == "" {
		if cfg := d.config(); cfg != nil {
			provider = cfg.DefaultProviderName()
		}
	}
	wd := strings.TrimSpace(t.WorkingDir)
	additional := append([]string(nil), t.AdditionalDirs...)
	projectID := ""
	if t.ProjectID != "" {
		if p, primary, ok := d.projects.drives(t.ProjectID); ok {
			wd = primary
			additional = append([]string(nil), p.AdditionalDirs...)
			projectID = p.ID
		}
	}
	if strings.TrimSpace(wd) == "" {
		wd = d.defaultWorkspaceFor()
	}

	sess, err := sm.New(provider, wd)
	if err != nil {
		return nil, fmt.Errorf("创建专属会话失败：%w", err)
	}
	sess.Title = job.Name
	sess.ProjectID = projectID
	sess.AdditionalDirs = additional
	sess.SkipApproval = t.SkipApproval
	// The job binds this conversation immediately after creation; protect it
	// from cleanup before the first run can even finish.
	sess.CronBound = true
	if err := sm.UpdateMeta(sess); err != nil {
		_ = sm.Delete(sess.ID)
		return nil, fmt.Errorf("写入专属会话失败：%w", err)
	}
	d.emitSessionsChanged(sess.ID)
	return sess, nil
}

// ---------------------------------------------------------------------------
// The trigger
// ---------------------------------------------------------------------------

func (d *desktopApp) onCronTrigger(ctx context.Context, job *cron.Job) error {
	if job == nil || job.ID == "" {
		return fmt.Errorf("定时任务缺少 id")
	}
	// The engine's closure captures the job as it was when the entry was ADDED.
	// A first-run binding (and any targeted edit that does not reschedule) only
	// rewrites the store, so the captured copy would still say Mode=new and
	// create a second dedicated conversation on every fire. Re-read the latest
	// record; it is the only copy the handler may act on.
	job = d.freshCronJob(job)
	if job == nil {
		return fmt.Errorf("定时任务已不存在")
	}
	if job.Desktop == nil {
		return fmt.Errorf("定时任务缺少桌面目标")
	}
	d.markCronRunning(job.ID, true)
	defer d.markCronRunning(job.ID, false)

	sessionID, err := d.resolveCronTarget(ctx, job)
	if err != nil {
		return err
	}
	return d.runCronTurn(ctx, sessionID, job)
}

// freshCronJob re-reads a job from the store. The scheduler's engine closure
// captures the record it added; a targeted write (a first-run binding) does not
// reschedule, so that captured copy goes stale. A handler acts on this one.
// nil means the job was deleted between the fire and this read.
func (d *desktopApp) freshCronJob(job *cron.Job) *cron.Job {
	if job == nil || job.ID == "" {
		return job
	}
	if sched := d.cronScheduler(); sched != nil {
		fresh, err := sched.Get(job.ID)
		if err == nil {
			return fresh
		}
	}
	return job
}

// runCronTurn runs one scheduled turn and does not return until it is over.
// It waits for a user's turn to finish first (a conversation runs one turn at
// a time), then runs with laneCron so the transcript and the completion
// notification behave like a normal turn.
func (d *desktopApp) runCronTurn(ctx context.Context, sessionID string, job *cron.Job) error {
	logicalID := job.Desktop.SessionID

	r, err := d.prepareSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("准备会话失败：%w", err)
	}

	// Wait for the conversation to be free. Re-resolve the leaf after every
	// wait: the user's turn may have compacted the conversation into a child.
	for {
		d.mu.Lock()
		run := d.ownerOfLocked(sessionID)
		busy := run != nil && run.running
		var done chan struct{}
		if run != nil {
			done = run.turnDone
		}
		d.mu.Unlock()

		if !busy {
			break
		}
		if done != nil {
			select {
			case <-done:
			case <-ctx.Done():
				return fmt.Errorf("等待会话空闲超时：%w", ctx.Err())
			}
		} else {
			select {
			case <-time.After(200 * time.Millisecond):
			case <-ctx.Done():
				return fmt.Errorf("等待会话空闲超时：%w", ctx.Err())
			}
		}
		if logicalID != "" {
			leaf, lerr := d.leafSession(logicalID)
			if lerr != nil {
				return lerr
			}
			if leaf.ID != sessionID {
				sessionID = leaf.ID
				r, err = d.prepareSession(ctx, sessionID)
				if err != nil {
					return fmt.Errorf("准备会话失败：%w", err)
				}
				if r.agent == nil {
					return fmt.Errorf("agent 不可用（未配置）")
				}
			}
		}
	}
	if r.agent == nil {
		return fmt.Errorf("agent 不可用（未配置）")
	}

	turnCtx, _, _, turnDone, ok := d.beginTurnWith(ctx, sessionID, r)
	if !ok {
		return fmt.Errorf("会话正在运行，本次跳过")
	}
	endReason := "error"
	defer func() {
		d.endTurn(sessionID, endReason)
		close(turnDone)
	}()

	// A scheduled turn is NOT steerable. Clear the channel beginTurn put on the
	// run so Steer refuses, and never hand it to the agent: the user's queued
	// message must not silently change what the task was asked to do. It goes
	// out as the next turn when this one completes.
	d.mu.Lock()
	if run := d.getRun(sessionID); run != nil {
		run.steerCh = nil
		run.cronJobName = job.Name
		run.cronSilent = false
	}
	d.mu.Unlock()

	// The conversation's approval mode decides how an ask rule behaves here.
	// "无需审批" means the agent's session-wide switch is on and no card is
	// raised; otherwise nobody can answer one, so the command is refused.
	cur := r.sm.Current()
	if cur == nil {
		return fmt.Errorf("会话记录不可用")
	}
	turnCtx = cronTurnContext(turnCtx, cur)

	d.setSessionState(sessionID, AgentState{Status: StatusThinking, Label: "执行", Detail: "定时任务"})

	display := cronDisplayText(job)
	modelText := cronModelPrompt(job)
	expanded := atfile.Expand(d.expansionRoot(r), modelText, atfile.Options{Sessions: d.sessionSource()})
	history := d.runHistory(sessionID)
	history, modelText = mergeTrailingUserMessage(history, expanded.Text)

	if d.app != nil {
		d.app.Event.Emit(cronTurnEventName, CronTurnEvent{
			SessionID: sessionID,
			JobID:     job.ID,
			JobName:   job.Name,
			Schedule:  job.Schedule,
			Display:   display,
		})
	}

	ropts := []agent.RunOption{
		agent.WithDisplayUserMessage(display),
		agent.WithExtraTools(
			d.newDesktopCronTool(sessionID),
			tools.NewSendFileTool(),
		),
	}
	if len(expanded.Images) > 0 {
		ropts = append(ropts, agent.WithPendingImages(expanded.Images))
	}

	var text strings.Builder
	var runErr error
	stream := r.agent.RunConversationStream(
		turnCtx, history, modelText, d.systemPromptFor(sessionID),
		llm.ChatOptions{MaxTokens: d.config().MaxTokens},
		ropts...,
	)
	for ev := range stream {
		switch ev.Type {
		case agent.AgentEventTextDelta:
			text.WriteString(ev.TextDelta)
		case agent.AgentEventTurnComplete:
			endReason = "complete"
			if job.EffectiveNotify() == cron.NotifyWhenRelevant {
				result := text.String()
				if ev.Messages != nil {
					if last := lastAssistantText(ev.Messages); strings.TrimSpace(last) != "" {
						result = last
					}
				}
				silent := job.ShouldSuppressResult(result)
				// Set BEFORE handleEventIn: its TurnComplete branch reads this
				// to decide whether to post the notification.
				d.mu.Lock()
				if run := d.runs[sessionID]; run != nil {
					run.cronSilent = silent
				}
				d.mu.Unlock()
			}
		case agent.AgentEventError:
			if ev.Result != nil {
				if ev.Result.ExitReason == agent.ExitReasonInterrupted ||
					ev.Result.ExitReason == agent.ExitReasonCancelled {
					endReason = "interrupted"
				}
				if ev.Result.Error != nil {
					runErr = ev.Result.Error
				}
			}
		}
		d.handleEventIn(sessionID, ev, laneCron)
	}

	if runErr != nil {
		return runErr
	}
	switch endReason {
	case "complete", "interrupted":
		// A stop the user asked for is a conclusion, not a failure.
		return nil
	default:
		return fmt.Errorf("定时任务没有完成")
	}
}

// cronTurnContext applies the target conversation's approval mode to a
// scheduled run. "需要审批" means nobody can answer a card, so the run is marked
// no-ask and an ask rule becomes a refusal; "无需审批" leaves the ctx alone —
// the agent's session-wide switch (applied when it was built, and again by
// SetSkipApproval) makes the ask rules silent.
func cronTurnContext(ctx context.Context, sess *session.Session) context.Context {
	if sess == nil || !sess.SkipApproval {
		return withoutAsk(ctx)
	}
	return ctx
}

// cronModelPrompt is what the model sees: the job's own words, the
// when_relevant policy, and the unattended-run instruction. No timestamp — the
// text has to be byte-identical across runs to keep the prompt cache.
func cronModelPrompt(job *cron.Job) string {
	text := job.Prompt
	if job.EffectiveNotify() == cron.NotifyWhenRelevant {
		text += "\n\n" + cron.MarkerNotificationPolicy
	}
	return text + "\n\n" + cronUnattendedPrompt
}

// cronDisplayText is what the conversation shows. It is stored as the turn's
// display_content, so a reloaded transcript shows the same bubble.
func cronDisplayText(job *cron.Job) string {
	prompt := strutil.Truncate(strings.TrimSpace(job.Prompt), cronDisplayPromptMaxRune)
	if prompt == "" {
		return "⏰ " + job.Name
	}
	return "⏰ " + job.Name + "\n" + prompt
}

func lastAssistantText(msgs []llm.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			return msgs[i].Content
		}
	}
	return ""
}

// newDesktopCronTool binds the Cron tool to one conversation AND tells the
// frontend when it changes something. The count on the composer's clock and on
// the session row is derived from crons.json, so a task created or deleted by
// the model during a turn must refresh the session list immediately — not on
// the next launch.
func (d *desktopApp) newDesktopCronTool(sessionID string) *tools.CronTool {
	tool := tools.NewCronTool(d.cronScheduler(), d.cronToolTarget(sessionID))
	tool.SetOnChange(func() {
		d.emitCronChanged("")
		d.syncCronProtection()
	})
	return tool
}

// cronToolTarget binds the Cron tool to one desktop conversation. It is used by
// ordinary turns (so the model can schedule work) and by a scheduled turn (so
// one task can create the next one).
func (d *desktopApp) cronToolTarget(sessionID string) tools.CronTarget {
	logical := d.logicalIDFor(sessionID)
	return tools.CronTarget{
		Type: cron.TargetTypeDesktop,
		ID:   func() string { return logical },
		// The same logical id is what a job's target carries, so the tool can
		// tell the model a job writes here instead of leaving it to compare the
		// target id with the (post-compaction, different) session id.
		SelfID: func() string { return logical },

		Matches: func(j *cron.Job) bool {
			if j.TargetType != cron.TargetTypeDesktop || j.Desktop == nil {
				return false
			}
			// Jobs that write into this conversation, plus dedicated ones this
			// conversation created (CreatedBy survives the binding).
			return j.Desktop.SessionID == logical || j.CreatedBy == logical
		},
		Prepare: func(j *cron.Job, mode string) error {
			j.TargetType = cron.TargetTypeDesktop
			j.CreatedBy = logical
			if mode == "new" {
				t := &cron.DesktopTarget{Mode: cron.DesktopModeNew}
				if cur := d.sessionRecord(sessionID); cur != nil {
					t.ProjectID = cur.ProjectID
					t.WorkingDir = cur.WorkingDir
					t.AdditionalDirs = append([]string(nil), cur.AdditionalDirs...)
					t.ProviderName = cur.ProviderName
					t.SkipApproval = cur.SkipApproval
				}
				j.Desktop = t
				return nil
			}
			j.Desktop = &cron.DesktopTarget{Mode: cron.DesktopModeSession, SessionID: logical}
			return nil
		},
		SupportsNewSession: true,
	}
}

// logicalIDFor answers "which conversation is this session a page of?".
func (d *desktopApp) logicalIDFor(sessionID string) string {
	if s := d.sessionRecord(sessionID); s != nil {
		return s.LogicalConversationID()
	}
	return sessionID
}

// currentLogicalID is the active conversation's logical id ("" when nothing is
// displayed).
func (d *desktopApp) currentLogicalID() string {
	id := d.currentID()
	if id == "" {
		return ""
	}
	return d.logicalIDFor(id)
}

// ---------------------------------------------------------------------------
// Bindings: the panel and the editor
// ---------------------------------------------------------------------------

// ListCronJobs returns the desktop jobs, with their target's current title and
// the next/last run already resolved. Channel jobs are not listed: this panel
// cannot deliver them and must not edit them.
func (s *AgentService) ListCronJobs() []CronJobVO {
	d := s.desk
	jobs, err := d.cronJobRecords()
	if err != nil {
		return nil
	}
	leaves := d.leafSessionsByLogical()
	activeID := d.currentID()

	out := make([]CronJobVO, 0, len(jobs))
	for _, job := range jobs {
		if job.TargetType != cron.TargetTypeDesktop || job.Desktop == nil {
			continue
		}
		vo := CronJobVO{
			ID:           job.ID,
			Name:         job.Name,
			Schedule:     job.Schedule,
			ScheduleText: cron.DescribeSchedule(job.Schedule),
			Type:         string(job.Type),
			Prompt:       job.Prompt,
			Status:       string(job.Status),
			Notify:       string(job.EffectiveNotify()),
			Timezone:     job.Timezone,
			LastRunAt:    job.LastRunAt,
			LastRunState: job.LastRunStatus,
			LastRunError: job.LastRunError,
			CreatedAt:    job.CreatedAt,
			UpdatedAt:    job.UpdatedAt,
		}
		if job.Desktop.SessionID == "" && job.Desktop.Mode == cron.DesktopModeNew {
			vo.TargetKind = "pending"
			vo.TargetProjectID = job.Desktop.ProjectID
			vo.TargetWorkingDir = job.Desktop.WorkingDir
			vo.TargetProvider = job.Desktop.ProviderName
			vo.TargetSkipApproval = job.Desktop.SkipApproval
		} else {
			vo.TargetSessionID = job.Desktop.SessionID
			if leaf := leaves[job.Desktop.SessionID]; leaf != nil {
				vo.TargetKind = "session"
				vo.TargetTitle = leaf.Title
				vo.TargetIsCurrent = leaf.ID == activeID
			} else {
				vo.TargetKind = "missing"
			}
		}
		if job.Status == cron.JobStatusActive {
			if next, nerr := cron.NextRun(job.Schedule, job.Timezone, time.Now()); nerr == nil {
				vo.NextRun = next
			} else {
				vo.NextRunError = nerr.Error()
			}
		}
		d.mu.Lock()
		vo.Running = d.cronRunning[job.ID]
		d.mu.Unlock()
		out = append(out, vo)
	}

	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		aa := a.Status == string(cron.JobStatusActive)
		ba := b.Status == string(cron.JobStatusActive)
		if aa != ba {
			return aa
		}
		if !a.NextRun.IsZero() && !b.NextRun.IsZero() && !a.NextRun.Equal(b.NextRun) {
			return a.NextRun.Before(b.NextRun)
		}
		if a.NextRun.IsZero() != b.NextRun.IsZero() {
			return !a.NextRun.IsZero()
		}
		return a.CreatedAt.Before(b.CreatedAt)
	})
	return out
}

// ListCronTargets returns the conversations a job can be bound to. Compaction
// chains collapse to their leaf; subsystem-owned sessions are left out (a
// scheduled task must not write into somebody else's conversation).
func (s *AgentService) ListCronTargets() []CronTargetVO {
	d := s.desk
	if d.sm == nil {
		return nil
	}
	list, err := d.sm.List()
	if err != nil {
		return nil
	}
	activeID := d.currentID()
	byLogical := make(map[string]*session.Session)
	for _, sess := range list {
		if sess.CompactedChildID != "" || sess.Managed() {
			continue
		}
		key := sess.LogicalConversationID()
		if prev := byLogical[key]; prev == nil || sess.UpdatedAt.After(prev.UpdatedAt) {
			byLogical[key] = sess
		}
	}
	out := make([]CronTargetVO, 0, len(byLogical))
	for _, sess := range byLogical {
		primary, _ := d.sessionRootsFrom(sess)
		vo := CronTargetVO{
			SessionID:    sess.LogicalConversationID(),
			Title:        sess.Title,
			ProjectID:    sess.ProjectID,
			WorkingDir:   primary,
			ProviderName: sess.ProviderName,
			SkipApproval: sess.SkipApproval,
			IsCurrent:    sess.ID == activeID,
			UpdatedAt:    sess.UpdatedAt,
		}
		if vo.Title == "" {
			vo.Title = "未命名会话"
		}
		if sess.ProjectID != "" {
			if p, ok := d.projects.get(sess.ProjectID); ok {
				vo.ProjectName = p.Name
			}
		}
		out = append(out, vo)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsCurrent != out[j].IsCurrent {
			return out[i].IsCurrent
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out
}

// PreviewCron validates a schedule/timezone pair and returns the next few
// occurrences. It is the editor's live feedback; an invalid pair is returned
// as Error, not as a failed binding.
func (s *AgentService) PreviewCron(schedule, timezone string) CronPreviewVO {
	out := CronPreviewVO{LocalTimezone: cron.LocalTimezoneName()}
	next, err := cron.NextRuns(schedule, timezone, time.Now(), cronPreviewCount)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	out.Next = next
	return out
}

// SaveCronJob creates (ID == "") or updates a desktop job. It answers "ok" or
// the reason it refused; the frontend re-reads the list on success.
func (s *AgentService) SaveCronJob(in CronJobInput) string {
	d := s.desk
	sched := d.cronScheduler()
	if sched == nil {
		return "定时任务已在设置里关闭，无法保存"
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return "任务名不能为空"
	}
	if strings.TrimSpace(in.Schedule) == "" {
		return "运行时间不能为空"
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return "任务内容不能为空"
	}
	if _, err := cron.NextRun(in.Schedule, in.Timezone, time.Now()); err != nil {
		return err.Error()
	}

	mode := in.Mode
	if mode == "" {
		mode = "session"
	}
	target := &cron.DesktopTarget{Mode: cron.DesktopModeSession}
	switch mode {
	case "session":
		if strings.TrimSpace(in.SessionID) == "" {
			return "请选择一个会话"
		}
		leaf, err := d.leafSession(in.SessionID)
		if err != nil {
			return "目标会话已不存在，请重新选择"
		}
		target.SessionID = leaf.LogicalConversationID()
	case "new":
		target.Mode = cron.DesktopModeNew
		target.ProjectID = strings.TrimSpace(in.ProjectID)
		target.WorkingDir = strings.TrimSpace(in.WorkingDir)
		target.AdditionalDirs = append([]string(nil), in.AdditionalDirs...)
		target.ProviderName = strings.TrimSpace(in.ProviderName)
		target.SkipApproval = in.SkipApproval
	default:
		return "回复方式不认识"
	}

	jobType := cron.JobType(in.Type)
	if jobType == "" {
		jobType = cron.JobTypeRecurring
	}
	notify := cron.NotifyPolicy(in.Notify)
	if notify == "" {
		notify = cron.NotifyAlways
	}

	if strings.TrimSpace(in.ID) == "" {
		createdFrom := d.currentLogicalID()
		job := &cron.Job{
			Name:       name,
			Schedule:   in.Schedule,
			Prompt:     in.Prompt,
			Type:       jobType,
			Notify:     notify,
			Timezone:   strings.TrimSpace(in.Timezone),
			TargetType: cron.TargetTypeDesktop,
			Desktop:    target,
			CreatedBy:  createdFrom,
		}
		created, err := sched.Create(job)
		if err != nil {
			return err.Error()
		}
		d.syncCronProtection()
		d.emitCronChanged(created.ID)
		return "ok:" + created.ID
	}

	job, err := sched.Get(in.ID)
	if err != nil {
		return err.Error()
	}
	if job == nil {
		return "任务不存在"
	}
	if job.TargetType != cron.TargetTypeDesktop {
		return "这个任务不属于 desktop"
	}
	updated, err := sched.Update(in.ID, cron.UpdateOpts{
		Name:     &name,
		Schedule: &in.Schedule,
		Prompt:   &in.Prompt,
		Type:     &jobType,
		Notify:   &notify,
		Timezone: &in.Timezone,
		Desktop:  target,
	})
	if err != nil {
		return err.Error()
	}
	d.syncCronProtection()
	d.emitCronChanged(updated.ID)
	return "ok:" + updated.ID
}

func (s *AgentService) DeleteCronJob(id string) string {
	d := s.desk
	sched := d.cronScheduler()
	if sched == nil {
		return "定时任务已在设置里关闭"
	}
	job, err := sched.Get(id)
	if err != nil {
		return err.Error()
	}
	if job == nil {
		return "任务不存在"
	}
	if job.TargetType != cron.TargetTypeDesktop {
		return "这个任务不属于 desktop"
	}
	if err := sched.Delete(id); err != nil {
		return err.Error()
	}
	d.syncCronProtection()
	d.emitCronChanged(id)
	return "ok"
}

func (s *AgentService) SetCronJobPaused(id string, paused bool) string {
	d := s.desk
	sched := d.cronScheduler()
	if sched == nil {
		return "定时任务已在设置里关闭"
	}
	job, err := sched.Get(id)
	if err != nil {
		return err.Error()
	}
	if job == nil {
		return "任务不存在"
	}
	if job.TargetType != cron.TargetTypeDesktop {
		return "这个任务不属于 desktop"
	}
	if paused {
		_, err = sched.Pause(id)
	} else {
		_, err = sched.Resume(id)
	}
	if err != nil {
		return err.Error()
	}
	d.emitCronChanged(id)
	return "ok"
}

// RunCronJobNow triggers one job immediately. It does not change the schedule;
// the run itself is asynchronous.
func (s *AgentService) RunCronJobNow(id string) string {
	d := s.desk
	sched := d.cronScheduler()
	if sched == nil {
		return "定时任务已在设置里关闭"
	}
	job, err := sched.Get(id)
	if err != nil {
		return err.Error()
	}
	if job == nil {
		return "任务不存在"
	}
	if job.TargetType != cron.TargetTypeDesktop {
		return "这个任务不属于 desktop"
	}
	if err := sched.RunNow(id); err != nil {
		return err.Error()
	}
	return "ok"
}

// IsCronScheduled reports whether the desktop scheduler is running; the panel
// uses it to say "已在设置里关闭" instead of leaving a silent empty list.
func (s *AgentService) IsCronScheduled() bool {
	return s.desk.cronScheduler() != nil
}
