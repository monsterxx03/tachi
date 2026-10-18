package main

// Desktop-side AutoDream: the background memory consolidation pass.
//
// channel runs dream from a cron entry at a fixed hour. A desktop app is usually
// not running at that hour, and the hours it IS open are exactly when there is
// new conversation to consolidate — so the desktop asks periodically whether a
// domain is due instead of naming an hour. Asking is cheap (a session list plus
// a state file per domain); dream.min_interval, applied by the orchestrator,
// is what bounds how often the question turns into an LLM run.
//
// The same pass backs the /dream command, so the button and the timer cannot
// disagree about what "consolidate now" means.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/agent/memory"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/dream"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/session"
)

const (
	// desktopDreamFirstCheck waits past startup before the first pass: providers,
	// sessions and the config are being loaded then, and consolidation is never
	// urgent enough to compete with opening a window.
	desktopDreamFirstCheck = 2 * time.Minute
	// desktopDreamCheckInterval is the TICK, not the run rate:
	// dream.min_interval decides whether a tick becomes a run.
	desktopDreamCheckInterval = 30 * time.Minute
	// desktopDreamPassTimeout bounds one pass across all domains, so a provider
	// that never answers cannot leave the periodic loop stuck forever.
	desktopDreamPassTimeout = 30 * time.Minute
	// desktopDreamShutdownWait is how long quitting waits for an in-flight pass.
	// A pass whose provider ignores cancellation must not hold up the quit; the
	// process is going away either way.
	desktopDreamShutdownWait = 5 * time.Second
)

// startDream starts the periodic pass when dream is enabled. Like startCron it
// runs after initAgent, because it reads the config that just booted; with the
// feature off no goroutine exists at all.
func (d *desktopApp) startDream(ctx context.Context) {
	cfg := d.config()
	if cfg == nil || !cfg.Dream.Enabled {
		return
	}
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	d.dreamMu.Lock()
	d.dreamCancel = cancel
	d.dreamDone = done
	d.dreamMu.Unlock()

	go func() {
		defer close(done)
		timer := time.NewTimer(desktopDreamFirstCheck)
		defer timer.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-timer.C:
			}
			if _, err := d.runDreamPass(loopCtx, false); err != nil && loopCtx.Err() == nil {
				logger.New("desktop").Error(loopCtx, "dream: pass failed", err)
			}
			timer.Reset(desktopDreamCheckInterval)
		}
	}()
}

// stopDream cancels the periodic pass and waits for it to return. Called from
// OnShutdown before the process goes away.
func (d *desktopApp) stopDream() {
	d.dreamMu.Lock()
	cancel, done := d.dreamCancel, d.dreamDone
	d.dreamCancel, d.dreamDone = nil, nil
	d.dreamMu.Unlock()

	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-time.After(desktopDreamShutdownWait):
		logger.New("desktop").Warn(context.Background(), "dream: pass still running at shutdown")
	}
}

// dreamPassRunning reports whether a pass holds the app-level gate.
func (d *desktopApp) dreamPassRunning() bool {
	d.dreamMu.Lock()
	defer d.dreamMu.Unlock()
	return d.dreamBusy
}

func (d *desktopApp) beginDreamPass() bool {
	d.dreamMu.Lock()
	defer d.dreamMu.Unlock()
	if d.dreamBusy {
		return false
	}
	d.dreamBusy = true
	return true
}

func (d *desktopApp) endDreamPass() {
	d.dreamMu.Lock()
	d.dreamBusy = false
	d.dreamMu.Unlock()
}

// runDreamPass runs one consolidation pass over every memory domain and reports
// how many domains were actually dreamed.
//
// force is the /dream path: it bypasses dream.min_interval, because "整理一下
// 现在" means now, not "if the cooldown allows". The enabled switch is enforced
// by BOTH callers — the desktop does not consolidate memory the user has not
// turned on, automatically or on request.
func (d *desktopApp) runDreamPass(ctx context.Context, force bool) (int, error) {
	cfg := d.config()
	if cfg == nil {
		return 0, errors.New("配置未加载")
	}
	if !cfg.Dream.Enabled {
		return 0, errors.New("记忆整理未开启（dream.enabled）")
	}
	sm := d.newSessionManager()
	if sm == nil {
		return 0, errors.New("会话存储不可用")
	}
	sessions, err := sm.List()
	if err != nil {
		return 0, fmt.Errorf("读取会话列表失败：%w", err)
	}

	// The fallback provider is resolved HERE rather than borrowed from a session:
	// a pass is app-wide (every domain, active session or not), and the desktop
	// is allowed to run with no conversation open at all.
	name := cfg.DefaultProviderName()
	if name == "" {
		return 0, errors.New("没有配置模型 provider")
	}
	resolved, err := llm.BuildProvider(cfg, name)
	if err != nil {
		return 0, fmt.Errorf("解析模型 provider 失败：%w", err)
	}

	if !d.beginDreamPass() {
		return 0, nil // another pass is already running
	}
	defer d.endDreamPass()

	passCtx, cancel := context.WithTimeout(ctx, desktopDreamPassTimeout)
	defer cancel()

	minInterval := cfg.Dream.MinInterval
	if force {
		minInterval = 0
	}
	o := dream.NewOrchestrator(dream.Config{
		Logger:            logger.New("desktop"),
		MaxConcurrent:     cfg.Dream.MaxConcurrent,
		MinInterval:       minInterval,
		MaxSessionsPerRun: cfg.Dream.MaxSessionsPerRun,
	})

	// A fresh manager for reading: Load() repoints the manager's current
	// session, and the app's run managers must keep pointing at what the user
	// is looking at.
	loadMessages := func(id string) ([]session.Message, error) {
		if _, err := sm.Load(id); err != nil {
			return nil, err
		}
		return sm.LoadMessages()
	}

	return o.Run(passCtx, sessions, func(ctx context.Context, plan dream.Plan) (dream.State, error) {
		return dream.RunDream(ctx, plan, dream.RunConfig{
			FallbackProvider: resolved.Provider,
			DreamProvider:    cfg.Dream.Provider,
			Config:           cfg,
			MaxIter:          cfg.Dream.SubagentMaxIter,
			MaxTokens:        cfg.MaxTokens,
			MaxMessageChars:  cfg.Dream.MaxMessageChars,
			Logger:           logger.New("desktop"),
		}, loadMessages)
	})
}

// dreamStatusText answers `/dream status`: what the background job is doing and
// whether there is anything waiting for it. It reads the same files the pass
// reads — no separate bookkeeping that could disagree with what will run.
func (d *desktopApp) dreamStatusText() string {
	var b strings.Builder
	b.WriteString("AutoDream 状态\n")

	enabled := false
	if cfg := d.config(); cfg != nil {
		enabled = cfg.Dream.Enabled
	}
	if enabled {
		b.WriteString("自动整理：开（应用运行期间每 30 分钟检查一次）\n")
	} else {
		b.WriteString("自动整理：关（dream.enabled；关闭时 /dream 也不会整理）\n")
	}
	if d.dreamPassRunning() {
		b.WriteString("当前：正在整理\n")
	} else {
		b.WriteString("当前：空闲\n")
	}

	// The project domain follows the DISPLAYED session's tree (see memory's
	// projectRootFor); with nothing open only the global domain exists.
	workDir := ""
	if id := d.currentID(); id != "" {
		if sm := d.newSessionManager(); sm != nil {
			if sess, err := sm.Load(id); err == nil {
				workDir = sess.WorkingDir
			}
		}
	}

	type domain struct {
		name string
		root string
	}
	domains := []domain{{"全局", filepath.Join(config.BaseDir(), "memory")}}
	if p := memory.ProjectMemoryDir(workDir); p != "" {
		domains = append(domains, domain{"项目", p})
	}

	for _, dom := range domains {
		state := dream.LoadState(dom.root)
		inboxBytes := int64(0)
		if info, err := os.Stat(filepath.Join(dom.root, memory.InboxFile)); err == nil {
			inboxBytes = info.Size()
		}
		topicCount := 0
		if entries, err := os.ReadDir(filepath.Join(dom.root, "topics")); err == nil {
			for _, e := range entries {
				if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
					topicCount++
				}
			}
		}
		last := "从未整理"
		if !state.LastDreamAt.IsZero() {
			last = state.LastDreamAt.Format("2006-01-02 15:04")
		}
		fmt.Fprintf(&b, "- %s（%s）：上次整理 %s；待整理 %d 字节；主题文件 %d 个\n",
			dom.name, dom.root, last, inboxBytes, topicCount)
	}
	return b.String()
}
