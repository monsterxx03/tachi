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

// MCPParamItem describes a single tool parameter for the detail view.
type MCPParamItem struct {
	Name        string
	Type        string
	Description string
	Required    bool
}

// MCPToolItem is the display model for one tool exposed by an MCP server.
type MCPToolItem struct {
	Name        string
	Description string
	Parameters  []MCPParamItem
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

// MCPView renders the MCP management overlay with server list, tool list, and tool detail.
//
// Navigation model (three levels):
//   1. Server list    — ↑↓ to select, Enter → enter tool list
//   2. Tool list      — ↑↓ to select, Enter → open detail, Esc → back to servers
//   3. Tool detail    — shows full description + parameter table, Esc → back to tool list
type MCPView struct {
	servers  []MCPServerItem
	selIdx   int

	// Tool list state
	focusOnTools     bool // true when cursor is in the tool list (not server list)
	toolSelIdx       int  // selected tool index within the current server
	toolScroll       int  // scroll offset for tool list / detail
	showingToolDetail bool // true when Enter was pressed on a tool to show its detail panel

	width  int
	height int

	message    string
	needAction MCPAction
}

// NewMCPView creates an empty MCPView.
func NewMCPView() MCPView {
	return MCPView{}
}

// SetServers replaces the server data and resets all selection state.
func (v *MCPView) SetServers(items []MCPServerItem) {
	v.servers = items
	if v.selIdx >= len(v.servers) {
		v.selIdx = 0
	}
	v.focusOnTools = false
	v.toolSelIdx = 0
	v.toolScroll = 0
	v.showingToolDetail = false
}

// SetSize records the bounding area for the overlay.
func (v *MCPView) SetSize(w, h int) {
	v.width = w
	v.height = h
}

// CursorUp moves selection up at the current focus level.
func (v *MCPView) CursorUp() {
	if v.focusOnTools {
		sel := v.SelectedServerItem()
		if sel != nil && v.toolSelIdx > 0 {
			v.toolSelIdx--
		}
	} else {
		if v.selIdx > 0 {
			v.selIdx--
		}
	}
	v.toolScroll = 0
}

// CursorDown moves selection down at the current focus level.
func (v *MCPView) CursorDown() {
	if v.focusOnTools {
		sel := v.SelectedServerItem()
		if sel != nil && v.toolSelIdx < len(sel.Tools)-1 {
			v.toolSelIdx++
		}
	} else {
		if v.selIdx < len(v.servers)-1 {
			v.selIdx++
		}
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

// focusedTool returns the currently selected tool within the current server, or nil.
func (v *MCPView) focusedTool() *MCPToolItem {
	sel := v.SelectedServerItem()
	if sel == nil || v.toolSelIdx < 0 || v.toolSelIdx >= len(sel.Tools) {
		return nil
	}
	return &sel.Tools[v.toolSelIdx]
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
	case "enter":
		sel := v.SelectedServerItem()
		if v.showingToolDetail {
			// Already viewing tool detail — nothing more to open
		} else if v.focusOnTools {
			// In tool list, viewing selected tool → open detail
			if v.toolSelIdx >= 0 && sel != nil && v.toolSelIdx < len(sel.Tools) {
				v.showingToolDetail = true
			}
		} else if sel != nil && len(sel.Tools) > 0 {
			// In server list → enter tool list
			v.focusOnTools = true
			v.toolSelIdx = 0
			v.toolScroll = 0
		}
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
		if v.showingToolDetail {
			v.showingToolDetail = false
		} else if v.focusOnTools {
			v.focusOnTools = false
			v.toolSelIdx = 0
			v.toolScroll = 0
		} else {
			v.needAction = MCPActionDismiss
		}
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

	var b strings.Builder

	// Title
	b.WriteString(mcpOverlayTitle.Render("MCP Servers"))
	b.WriteString("  ")
	if v.showingToolDetail {
		b.WriteString(mcpOverlayHint.Render("↑↓ tool  Esc back  t/r/a ops"))
	} else if v.focusOnTools {
		b.WriteString(mcpOverlayHint.Render("↑↓ tool  Enter detail  Esc back  t/r/a ops"))
	} else {
		b.WriteString(mcpOverlayHint.Render("↑↓ nav  Enter tools  t toggle  r reconnect  a auth  Esc close"))
	}
	b.WriteString("\n")

	// Compute split: 35% server list, 65% tool area (of inner height)
	innerH := v.height - 2       // border lines
	constantH := 1 + 1 + 1       // title + tool-header + message
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
	for i := len(v.servers); i < serverArea; i++ {
		b.WriteString("\n")
	}

	// --- Tool section ---
	sel := v.SelectedServerItem()
	if sel != nil {
		b.WriteString(mcpToolHeader.Render(fmt.Sprintf("── %s tools ──", sel.Name)))
		b.WriteString("\n")

		if v.showingToolDetail && v.toolSelIdx >= 0 && v.toolSelIdx < len(sel.Tools) {
			v.renderToolDetail(&b, &sel.Tools[v.toolSelIdx])
		} else if v.focusOnTools {
			v.renderToolList(&b, sel.Tools, toolArea, true) // interactive — show cursor
		} else {
			v.renderToolList(&b, sel.Tools, toolArea, false) // read-only — no cursor
		}
	} else {
		for i := 0; i < toolArea; i++ {
			b.WriteString("\n")
		}
	}

	// --- Message bar ---
	msg := v.message
	if msg == "" {
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

	// Wrap in border with MaxWidth to prevent CJK text overflow.
	bordered := mcpOverlayBorder.Copy().MaxWidth(v.width).Render(b.String())

	return lipgloss.Place(v.width, v.height, lipgloss.Center, lipgloss.Center, bordered)
}

func (v *MCPView) renderServerLine(i int, srv MCPServerItem) string {
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

	typeBadge := fmt.Sprintf("%-5s", srv.Type)

	toolStr := fmt.Sprintf("%d tools", srv.ToolCount)
	if !srv.Enabled || !srv.Connected {
		toolStr = "—"
	}

	oauthBadge := ""
	if srv.HasOAuth {
		oauthBadge = " OAuth"
	}

	profileBadge := ""
	if srv.Profile != "" {
		profileBadge = " [" + srv.Profile + "]"
	}

	// Build suffix: transport + spacing + tool count + oauth badge + profile badge
	suffix := fmt.Sprintf("%s  %s%s%s", typeBadge, toolStr, oauthBadge, profileBadge)
	// suffix text is plain ASCII (no ANSI) so len(suffix) == visual width

	innerW := v.width - 6
	if innerW < 20 {
		innerW = 20
	}
	// prefix = icon + space + " " before name
	prefixW := 3
	nameW := innerW - prefixW - len(suffix) - 1 // -1 for space between name and suffix
	if nameW < 3 {
		nameW = 3
	}

	name := srv.Name
	if len(name) > nameW {
		name = name[:nameW-1] + "…"
	}

	line := fmt.Sprintf("%s %-*s %s",
		icon, nameW, name, suffix)

	if i == v.selIdx && !v.focusOnTools {
		return mcpServerSelected.Render(line)
	}
	return style.Render(line)
}

// renderToolList renders the compact tool list (name + one-line description).
// When interactive is true, the cursor (▶) and selection highlight are shown.
func (v *MCPView) renderToolList(b *strings.Builder, tools []MCPToolItem, maxLines int, interactive bool) {
	const nameW = 24
	const descMaxRunes = 50 // truncate descriptions to keep list compact
	gap := 2

	shown := 0
	for i := v.toolScroll; i < len(tools) && shown < maxLines; i++ {
		t := tools[i]

		toolName := t.Name
		if len(toolName) > nameW {
			toolName = toolName[:nameW-1] + "…"
		}
		toolNameStyled := mcpToolName.Render(toolName)

		// Take first line only, then truncate to descMaxRunes runes
		desc := t.Description
		if nl := strings.IndexByte(desc, '\n'); nl >= 0 {
			desc = desc[:nl]
		}
		descRunes := []rune(desc)
		if len(descRunes) > descMaxRunes {
			desc = string(descRunes[:descMaxRunes-1]) + "…"
		}
		descStyled := dimStyle.Render(desc)

		cursor := " "
		if interactive && i == v.toolSelIdx {
			cursor = "▶"
		}

		line := cursor +
			padRight(toolNameStyled, nameW+cursorOffset(cursor)) +
			strings.Repeat(" ", gap) +
			descStyled

		if interactive && i == v.toolSelIdx {
			line = mcpServerSelected.Render(line)
		}

		b.WriteString(line)
		b.WriteString("\n")
		shown++
	}

	for shown < maxLines {
		b.WriteString("\n")
		shown++
	}
}

// cursorOffset returns the visual width added by the cursor prefix ("▶" = 1 rune, " " = 1 rune).
// Used to compensate in padRight so the name column stays aligned regardless of cursor.
func cursorOffset(cursor string) int {
	return lipgloss.Width(cursor)
}

// padRight pads s on the right so its visual width equals targetW, using spaces.
func padRight(s string, targetW int) string {
	w := lipgloss.Width(s)
	if w >= targetW {
		return s
	}
	return s + strings.Repeat(" ", targetW-w)
}

// renderToolDetail renders full description + parameter table for one tool.
func (v *MCPView) renderToolDetail(b *strings.Builder, t *MCPToolItem) {
	// Available text width inside border+padding:
	//   border = 2 chars, padding = 2×2 = 4 chars → overhead = 6
	//   Each line has "  " prefix = 2 chars
	//   → wrapWidth = v.width - 8
	wrapWidth := v.width - 8
	if wrapWidth < 20 {
		wrapWidth = 20
	}

	// Tool name
	b.WriteString(mcpDetailFieldName.Render(fmt.Sprintf("  %s", t.Name)))
	b.WriteString("\n")
	b.WriteString("  ")
	b.WriteString(strings.Repeat("─", wrapWidth))
	b.WriteString("\n")

	// Full description — preserve original text, just word-wrap
	desc := strings.TrimSpace(t.Description)
	for _, line := range wrapLines(desc, wrapWidth) {
		b.WriteString("  ")
		b.WriteString(mcpDetailFieldDesc.Render(line))
		b.WriteString("\n")
	}
	b.WriteString("\n")

	// Parameter table
	if len(t.Parameters) > 0 {
		b.WriteString("  ")
		b.WriteString(mcpDetailColHeader.Render("Parameters"))
		b.WriteString("\n")
		b.WriteString("  ")
		b.WriteString(strings.Repeat("─", wrapWidth))
		b.WriteString("\n")

		for _, p := range t.Parameters {
			// Parameter name + type + required
			req := ""
			if p.Required {
				req = " " + mcpStatusOK.Render("required")
			} else {
				req = " " + dimStyle.Render("optional")
			}
			header := fmt.Sprintf("  %s  %s%s",
				mcpDetailFieldName.Render(p.Name),
				mcpDetailFieldType.Render(p.Type),
				req)
			b.WriteString(header)
			b.WriteString("\n")

			// Parameter description — full text, wrapped with indent
			if p.Description != "" {
				pDesc := strings.TrimSpace(p.Description)
				for _, line := range wrapLines(pDesc, wrapWidth-2) { // -2 for extra indent
					b.WriteString("    ") // 4-space indent under param name
					b.WriteString(mcpDetailFieldDesc.Render(line))
					b.WriteString("\n")
				}
			}
			b.WriteString("\n")
		}
	}
}

// wrapLines splits text into lines no wider than maxW visual columns.
// Uses lipgloss.Width to correctly measure CJK characters (2 columns each).
func wrapLines(text string, maxW int) []string {
	var lines []string
	runes := []rune(text)

	for len(runes) > 0 {
		// Find how many runes fit within maxW columns
		var end int
		var w int
		for end = 0; end < len(runes); end++ {
			cw := runeWidth(runes[end])
			if w+cw > maxW {
				break
			}
			w += cw
		}
		if end == 0 {
			// Single rune wider than maxW — force one rune
			end = 1
		}
		// Try to break backward at a space within the last third
		cut := end
		if end < len(runes) {
			for i := end; i > end*2/3 && i > 0; i-- {
				if runes[i-1] == ' ' {
					cut = i
					break
				}
			}
		}
		line := strings.TrimRight(string(runes[:cut]), " ")
		lines = append(lines, line)
		// Skip leading spaces on next chunk
		runes = runes[cut:]
		for len(runes) > 0 && runes[0] == ' ' {
			runes = runes[1:]
		}
	}
	if len(lines) == 0 {
		lines = append(lines, "")
	}
	return lines
}

// runeWidth returns the terminal column width of a single rune.
func runeWidth(r rune) int {
	return lipgloss.Width(string(r))
}