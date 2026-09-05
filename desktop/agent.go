package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// AgentStatus enumerates the (simulated) runtime states the agent can be in.
// These mirror the states we will eventually derive from the real AgentEvent
// stream (idle / thinking / tool_running / busy / error).
type AgentStatus string

const (
	StatusIdle        AgentStatus = "idle"
	StatusThinking    AgentStatus = "thinking"
	StatusToolRunning AgentStatus = "tool_running"
	StatusBusy        AgentStatus = "busy"
	StatusError       AgentStatus = "error"
)

// AgentState is the payload pushed to the frontend (and shown in the menu
// bar). Keep the labels short enough for the macOS menu bar.
type AgentState struct {
	Status AgentStatus `json:"status"`
	Label  string      `json:"label"`  // short menu-bar text, e.g. "思考"
	Detail string      `json:"detail"` // one-line human description
}

// AgentService is a Wails-bound service. In this skeleton it drives a
// *simulated* agent lifecycle so we can preview the UI. The methods are the
// exact surface the real driver will expose once agent loop is wired up.
type AgentService struct {
	desk *desktopApp
}

// GetState returns the current agent state.
func (s *AgentService) GetState() AgentState {
	return s.desk.currentState()
}

// SendMessage starts a simulated turn. It returns immediately; state changes
// are streamed to the frontend via the "agent:state" event and to the menu bar.
func (s *AgentService) SendMessage(text string) string {
	if len(text) == 0 {
		return "empty"
	}
	s.desk.startSimulatedTurn(context.Background(), text)
	return "ok"
}

// Stop aborts the current simulated turn and returns to idle.
func (s *AgentService) Stop() string {
	s.desk.stopSimulatedTurn()
	return "ok"
}

// desktopApp holds everything that outlives a single request: the Wails app,
// the main window and the menu-bar tray. It also owns the (simulated) agent
// state and the goroutine that broadcasts it.
type desktopApp struct {
	app    *application.App
	tray   *application.SystemTray
	window *application.WebviewWindow

	mu     sync.Mutex
	state  AgentState
	simCh  chan struct{} // close to stop the current simulated turn
	quitCh chan struct{}
}

func newDesktopApp() *desktopApp {
	return &desktopApp{
		state:  AgentState{Status: StatusIdle, Label: "空闲", Detail: "就绪"},
		quitCh: make(chan struct{}),
	}
}

func (d *desktopApp) currentState() AgentState {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state
}

func (d *desktopApp) setState(st AgentState) {
	d.mu.Lock()
	d.state = st
	d.mu.Unlock()

	// Reflect on the menu bar: label text + status icon.
	if d.tray != nil {
		d.tray.SetLabel(st.Label)
		if icon := trayIcon(st.Status); icon != nil {
			d.tray.SetTemplateIcon(icon)
		}
	}
	// Push to the frontend.
	if d.app != nil {
		d.app.Event.Emit("agent:state", st)
	}
}

// startSimulatedTurn runs a scripted sequence of states so the UI and menu bar
// can be previewed without a real agent loop. Stopped via stopSimulatedTurn.
func (d *desktopApp) startSimulatedTurn(ctx context.Context, _ string) {
	d.mu.Lock()
	if d.simCh != nil {
		select {
		case <-d.simCh:
		default:
			// A turn is already running — ignore.
			d.mu.Unlock()
			return
		}
	}
	stop := make(chan struct{})
	d.simCh = stop
	d.mu.Unlock()

	sequence := []AgentState{
		{Status: StatusThinking, Label: "思考", Detail: "理解用户意图…"},
		{Status: StatusToolRunning, Label: "执行", Detail: "调用工具 grep"},
		{Status: StatusBusy, Label: "处理", Detail: "组织回答…"},
	}

	go func() {
		for _, st := range sequence {
			select {
			case <-stop:
				d.setState(AgentState{Status: StatusIdle, Label: "空闲", Detail: "已停止"})
				d.endSimulatedTurn(stop)
				return
			case <-time.After(1400 * time.Millisecond):
			}
			d.setState(st)
		}
		// On completion, settle to idle talking about the (fake) result.
		select {
		case <-stop:
		case <-time.After(1200 * time.Millisecond):
		}
		d.setState(AgentState{Status: StatusIdle, Label: "空闲", Detail: "已完成回答"})
		d.endSimulatedTurn(stop)
	}()
}

func (d *desktopApp) stopSimulatedTurn() {
	d.mu.Lock()
	stop := d.simCh
	d.mu.Unlock()
	if stop != nil {
		select {
		case <-stop:
		default:
			close(stop)
		}
	}
}

func (d *desktopApp) endSimulatedTurn(stop chan struct{}) {
	d.mu.Lock()
	if d.simCh == stop {
		d.simCh = nil
	}
	d.mu.Unlock()
}

func (d *desktopApp) shutdown() {
	close(d.quitCh)
}

// simulateRuntime ticks the state machine even when the user isn't interacting,
// so idle state is always reflected in the menu bar.
func (d *desktopApp) simulateRuntime() {
	ticker := time.NewTicker(8 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			// Idle heartbeat — keep the tray label honest.
			if d.currentState().Status == StatusIdle {
				d.setState(d.currentState())
			}
		case <-d.quitCh:
			return
		}
	}
}

// trayIcon returns the menu-bar (template) icon bytes for a given status, or
// nil to leave the current icon untouched.
func trayIcon(status AgentStatus) []byte {
	switch status {
	case StatusThinking:
		return iconThinking
	case StatusToolRunning:
		return iconTool
	case StatusBusy:
		return iconBusy
	case StatusError:
		return iconError
	default:
		return iconIdle
	}
}

// markTrayBuilt is a small hook used to catch misuse during development.
func (d *desktopApp) markTrayBuilt(t *application.SystemTray) {
	d.tray = t
}

var _ = fmt.Sprintf // keep fmt imported if unused later
