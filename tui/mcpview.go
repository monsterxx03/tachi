package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

// MCPServerItem is the display model for one MCP server in the overlay.
type MCPServerItem struct {
	Name      string
	Type      string // "stdio" or "http"
	Enabled   bool
	Connected bool
	ToolCount int
	Tools     []MCPToolItem
	HasOAuth  bool
	Profile   string // non-empty if from a profile
}

// MCPToolItem is the display model for one tool exposed by an MCP server.
type MCPToolItem struct {
	Name        string
	Description string // first-line summary, truncated to ~80 chars
}

// MCPAction represents a user-requested operation on a server.
type MCPAction int

const (
	MCPActionNone      MCPAction = iota
	MCPActionToggle              // t
	MCPActionReconnect           // r
	MCPActionAuth                // a
	MCPActionDismiss             // Esc / q
)

// MCPView renders the MCP management overlay with server list and expandable tool list.
type MCPView struct {
	servers    []MCPServerItem
	selIdx     int
	toolScroll int
	width      int
	height     int
	message    string
	needAction MCPAction // set by HandleKey, consumed by Model
}

// NewMCPView creates an empty MCPView.
func NewMCPView() MCPView {
	return MCPView{}
}

// SetServers replaces the server data and resets selection.
func (v *MCPView) SetServers(items []MCPServerItem) {
	v.servers = items
	if v.selIdx >= len(v.servers) {
		v.selIdx = 0
	}
	v.toolScroll = 0
}

// SetSize records the bounding area for the overlay.
func (v *MCPView) SetSize(w, h int) {
	v.width = w
	v.height = h
}

// CursorUp moves selection up.
func (v *MCPView) CursorUp() {
	if v.selIdx > 0 {
		v.selIdx--
	}
	v.toolScroll = 0
}

// CursorDown moves selection down.
func (v *MCPView) CursorDown() {
	if v.selIdx < len(v.servers)-1 {
		v.selIdx++
	}
	v.toolScroll = 0
}

// SelectedServer returns the name of the currently highlighted server.
func (v *MCPView) SelectedServer() string {
	if v.selIdx < 0 || v.selIdx >= len(v.servers) {
		return ""
	}
	return v.servers[v.selIdx].Name
}

// SelectedServerItem returns the currently highlighted server item, or nil.
func (v *MCPView) SelectedServerItem() *MCPServerItem {
	if v.selIdx < 0 || v.selIdx >= len(v.servers) {
		return nil
	}
	return &v.servers[v.selIdx]
}

// SetMessage displays a one-shot message at the bottom of the overlay.
func (v *MCPView) SetMessage(msg string) {
	v.message = msg
}

// HandleKey processes a key press inside the overlay and returns the action to perform.
func (v *MCPView) HandleKey(key string) MCPAction {
	v.needAction = MCPActionNone

	switch key {
	case "up", "k":
		v.CursorUp()
	case "down", "j":
		v.CursorDown()
	case "t":
		v.needAction = MCPActionToggle
	case "r":
		v.needAction = MCPActionReconnect
	case "a":
		v.needAction = MCPActionAuth
	case "pgup", "ctrl+u":
		if v.toolScroll > 0 {
			v.toolScroll--
		}
	case "pgdown", "ctrl+d":
		v.toolScroll++
	case "esc", "q":
		v.needAction = MCPActionDismiss
	}

	return v.needAction
}

// View renders the full overlay as a centered bordered box.
func (v *MCPView) View() string {
	if len(v.servers) == 0 {
		return lipgloss.Place(
			v.width, v.height,
			lipgloss.Center, lipgloss.Center,
			mcpOverlayBorder.Render("No MCP servers configured."),
		)
	}

	// --- build inner content ---
	var b strings.Builder

	// Title
	b.WriteString(mcpOverlayTitle.Render("MCP Servers"))
	b.WriteString("  ")
	b.WriteString(mcpOverlayHint.Render("↑↓ nav  Enter expand  t toggle  r reconnect  a auth  Esc close"))
	b.WriteString("\n")

	// Compute split: 35% server list, 65% tool list (of inner height)
	innerH := v.height - 2       // border
	constantH := 1 + 1 + 1       // title + tool-header + message (3 fixed lines)
	availH := innerH - constantH
	if availH < 2 {
		availH = 2
	}
	serverArea := availH * 35 / 100
	if serverArea < 1 {
		serverArea = 1
	}
	toolArea := availH - serverArea
	if toolArea < 1 {
		toolArea = 1
	}

	// --- Server list ---
	for i, srv := range v.servers {
		if i >= serverArea {
			break
		}
		b.WriteString(v.renderServerLine(i, srv))
		b.WriteString("\n")
	}
	// Pad server area
	for i := len(v.servers); i < serverArea; i++ {
		b.WriteString("\n")
	}

	// --- Tool section ---
	sel := v.SelectedServerItem()
	if sel != nil {
		b.WriteString(mcpToolHeader.Render(fmt.Sprintf("── %s tools ──", sel.Name)))
		b.WriteString("\n")

		shown := 0
		for i := v.toolScroll; i < len(sel.Tools) && shown < toolArea; i++ {
			b.WriteString(v.renderToolLine(sel.Tools[i]))
			b.WriteString("\n")
			shown++
		}
		for shown < toolArea {
			b.WriteString("\n")
			shown++
		}
	} else {
		// No selection: fill with blank
		for i := 0; i < toolArea; i++ {
			b.WriteString("\n")
		}
	}

	// --- Message bar ---
	msg := v.message
	if msg == "" {
		// Default status line
		if sel != nil {
			if sel.Connected {
				msg = mcpStatusOK.Render(fmt.Sprintf("✓ %s connected, %d tool(s)", sel.Name, len(sel.Tools)))
			} else if sel.Enabled {
				msg = mcpStatusWarn.Render(fmt.Sprintf("⚠ %s enabled but not connected", sel.Name))
			} else {
				msg = dimStyle.Render(fmt.Sprintf("%s is disabled", sel.Name))
			}
		}
	}
	b.WriteString(msg)

	// Wrap in border and center
	content := mcpOverlayBorder.Render(b.String())

	return lipgloss.Place(
		v.width, v.height,
		lipgloss.Center, lipgloss.Center,
		content,
	)
}

func (v *MCPView) renderServerLine(i int, srv MCPServerItem) string {
	// Status icon
	icon := "⚪"
	style := dimStyle
	if srv.Enabled {
		if srv.Connected {
			icon = "🟢"
			style = mcpServerConnected
		} else {
			icon = "🔴"
			style = mcpServerDisconnected
		}
	}

	// Transport badge
	typeBadge := fmt.Sprintf("%-5s", srv.Type)

	// Tool count
	toolStr := fmt.Sprintf("%d tools", srv.ToolCount)
	if !srv.Enabled {
		toolStr = "—"
	} else if !srv.Connected {
		toolStr = "—"
	}

	// OAuth badge
	oauthBadge := ""
	if srv.HasOAuth {
		oauthBadge = " " + mcpOAuthBadge.Render("OAuth")
	}

	// Profile badge
	profileBadge := ""
	if srv.Profile != "" {
		profileBadge = " " + dimStyle.Render("["+srv.Profile+"]")
	}

	// Build the line
	innerW := v.width - 6 // border(2) + inner padding(4)
	if innerW < 20 {
		innerW = 20
	}
	nameW := innerW - 25 // 5(type) + 10(tools) + 5(oauth) + 5(spacing)
	if nameW < 5 {
		nameW = 5
	}
	// Truncate name
	name := srv.Name
	if len(name) > nameW {
		name = name[:nameW-1] + "…"
	}

	line := fmt.Sprintf("%s %-*s %s  %s%s%s",
		icon, nameW, name, typeBadge, toolStr, oauthBadge, profileBadge)

	if i == v.selIdx {
		return mcpServerSelected.Render(line)
	}
	return style.Render(line)
}

func (v *MCPView) renderToolLine(t MCPToolItem) string {
	innerW := v.width - 8 // border + inner padding
	if innerW < 20 {
		innerW = 20
	}
	nameW := 24
	descW := innerW - nameW - 2
	if descW < 10 {
		descW = 10
	}

	// Truncate name
	toolName := t.Name
	if len(toolName) > nameW {
		toolName = toolName[:nameW-1] + "…"
	}

	// Truncate description to one line
	desc := t.Description
	// Take first line only
	if nl := strings.IndexByte(desc, '\n'); nl >= 0 {
		desc = desc[:nl]
	}
	descRunes := []rune(desc)
	if len(descRunes) > descW {
		desc = string(descRunes[:descW-1]) + "…"
	}

	return fmt.Sprintf("  %-*s  %s",
		nameW, mcpToolName.Render(toolName), dimStyle.Render(desc))
}
