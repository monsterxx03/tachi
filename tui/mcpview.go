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
	Deferred    bool // true when tool is in deferred pool but not yet registered
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
//  1. Server list    — ↑↓ to select, Enter → enter tool list
//  2. Tool list      — ↑↓/jk to scroll, Enter → open detail for highlighted tool, Esc → back to servers
//  3. Tool detail    — ↑↓/jk to scroll full description + parameters, Esc → back to tool list
type MCPView struct {
	servers []MCPServerItem
	selIdx  int

	// Tool panel state
	focusOnTools      bool // true when in tool list or detail (not server list)
	showingToolDetail bool // true when Enter was pressed on a tool to show its detail panel
	detailToolIdx     int  // which tool's detail is being shown (only meaningful when showingToolDetail)
	toolScroll        int  // scroll offset: in tool list = index of first visible tool; in detail = line offset

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
	v.detailToolIdx = 0
	v.toolScroll = 0
	v.showingToolDetail = false
}

// SetSize records the bounding area for the overlay.
func (v *MCPView) SetSize(w, h int) {
	v.width = w
	v.height = h
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

// focusedTool returns the tool currently shown in detail, or nil.
func (v *MCPView) focusedTool() *MCPToolItem {
	sel := v.SelectedServerItem()
	if sel == nil || !v.showingToolDetail {
		return nil
	}
	if v.detailToolIdx < 0 || v.detailToolIdx >= len(sel.Tools) {
		return nil
	}
	return &sel.Tools[v.detailToolIdx]
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
		if v.showingToolDetail || v.focusOnTools {
			if v.toolScroll > 0 {
				v.toolScroll--
			}
		} else if v.selIdx > 0 {
			v.selIdx--
		}
	case "down", "j":
		if v.showingToolDetail || v.focusOnTools {
			v.toolScroll++
		} else if v.selIdx < len(v.servers)-1 {
			v.selIdx++
		}
	case "enter":
		sel := v.SelectedServerItem()
		if v.showingToolDetail {
			// Nothing — already viewing detail
		} else if v.focusOnTools {
			// Open detail for the tool at current scroll position
			if sel != nil && v.toolScroll >= 0 && v.toolScroll < len(sel.Tools) {
				v.detailToolIdx = v.toolScroll
				v.showingToolDetail = true
				v.toolScroll = 0
			}
		} else if sel != nil && len(sel.Tools) > 0 {
			// Enter tool list
			v.focusOnTools = true
			v.toolScroll = 0
		}
	case "t":
		v.needAction = MCPActionToggle
	case "r":
		v.needAction = MCPActionReconnect
	case "a":
		v.needAction = MCPActionAuth
	case "esc", "q":
		if v.showingToolDetail {
			v.showingToolDetail = false
			v.toolScroll = v.detailToolIdx
		} else if v.focusOnTools {
			v.focusOnTools = false
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
		b.WriteString(mcpOverlayHint.Render("↑↓/jk scroll  Esc back  t/r/a ops"))
	} else if v.focusOnTools {
		b.WriteString(mcpOverlayHint.Render("↑↓/jk scroll  Enter detail  Esc back  t/r/a ops"))
	} else {
		b.WriteString(mcpOverlayHint.Render("↑↓ nav  Enter tools  t toggle  r reconnect  a auth  Esc close"))
	}
	b.WriteString("\n")

	// Pre-wrap the message so that long lines (e.g. OAuth auth URLs with no
	// spaces) are hard-wrapped to fit the overlay width, and we know the
	// exact line count before sizing the server/tool areas.
	const maxMsgLines = 8
	innerW := max(
		// mirror renderServerLine's innerW
		v.width-6, 20)
	wrappedMsg, msgLineCount := v.prepareMessage(innerW, maxMsgLines)

	// Compute split: 35% server list, 65% tool area (of inner height)
	innerH := v.height - 2            // border lines
	constantH := 1 + 1 + msgLineCount // title + tool-header + message
	availH := max(innerH-constantH, 2)
	serverArea := max(availH*35/100, 1)
	toolArea := max(availH-serverArea, 1)

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

		if v.showingToolDetail && v.detailToolIdx >= 0 && v.detailToolIdx < len(sel.Tools) {
			v.renderToolDetail(&b, &sel.Tools[v.detailToolIdx], toolArea)
		} else if v.focusOnTools {
			v.renderToolList(&b, sel.Tools, toolArea, true) // interactive — show cursor
		} else {
			v.renderToolList(&b, sel.Tools, toolArea, false) // read-only — no cursor
		}
	} else {
		for range toolArea {
			b.WriteString("\n")
		}
	}

	// --- Message bar ---
	if wrappedMsg != "" {
		b.WriteString(wrappedMsg)
	} else {
		msg := ""
		if sel != nil {
			if sel.Connected {
				msg = mcpStatusOK.Render(fmt.Sprintf("✓ %s connected, %d tool(s)", sel.Name, len(sel.Tools)))
			} else if sel.Enabled {
				msg = mcpStatusWarn.Render(fmt.Sprintf("⚠ %s enabled but not connected", sel.Name))
			} else {
				msg = dimStyle.Render(fmt.Sprintf("%s is disabled", sel.Name))
			}
		}
		b.WriteString(msg)
	}

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

	innerW := max(v.width-6, 20)
	// prefix = icon + space + " " before name
	prefixW := 3
	nameW := max(
		// -1 for space between name and suffix
		innerW-prefixW-len(suffix)-1, 3)

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

		// Deferred indicator for tools not yet loaded into the LLM
		if t.Deferred {
			toolNameStyled += " " + dimStyle.Render("📦")
		}

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
		if interactive && i == v.toolScroll {
			cursor = "▶"
		}

		line := cursor +
			padRight(toolNameStyled, nameW+cursorOffset(cursor)) +
			strings.Repeat(" ", gap) +
			descStyled

		if interactive && i == v.toolScroll {
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

// renderToolDetail renders full description + parameter table for one tool
// into a line slice. The caller applies scroll + maxLines to show the visible portion.
func (v *MCPView) renderToolDetail(b *strings.Builder, t *MCPToolItem, maxLines int) {
	// Build all lines into a slice controlled by toolScroll
	var lines []string

	// Available text width inside border+padding
	wrapWidth := max(v.width-8, 20)

	// Tool name
	lines = append(lines, mcpDetailFieldName.Render(fmt.Sprintf("  %s", t.Name)))
	lines = append(lines, "  "+strings.Repeat("─", wrapWidth))

	// Full description
	desc := strings.TrimSpace(t.Description)
	for _, line := range wrapLines(desc, wrapWidth) {
		lines = append(lines, "  "+mcpDetailFieldDesc.Render(line))
	}
	lines = append(lines, "")

	// Parameter table
	if len(t.Parameters) > 0 {
		lines = append(lines, "  "+mcpDetailColHeader.Render("Parameters"))
		lines = append(lines, "  "+strings.Repeat("─", wrapWidth))

		for _, p := range t.Parameters {
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
			lines = append(lines, header)

			if p.Description != "" {
				pDesc := strings.TrimSpace(p.Description)
				for _, line := range wrapLines(pDesc, wrapWidth-2) {
					lines = append(lines, "    "+mcpDetailFieldDesc.Render(line))
				}
			}
			lines = append(lines, "")
		}
	}

	// Clamp scroll
	if v.toolScroll >= len(lines) {
		v.toolScroll = len(lines) - 1
	}
	if v.toolScroll < 0 {
		v.toolScroll = 0
	}

	// Output only the visible slice
	shown := 0
	for i := v.toolScroll; i < len(lines) && shown < maxLines; i++ {
		b.WriteString(lines[i])
		b.WriteString("\n")
		shown++
	}
	for shown < maxLines {
		b.WriteString("\n")
		shown++
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

// prepareMessage wraps v.message to fit within maxW columns and caps the
// result at maxLines lines. It returns the final string (empty when
// v.message is "") and the line count (≥1 even when message is empty, so
// the layout always reserves space for the default status line).
//
// Each raw line is passed through wrapLines so that long lines with no
// spaces (e.g. OAuth authorization URLs) are hard-wrapped at character
// boundaries rather than truncated.
func (v *MCPView) prepareMessage(maxW, maxLines int) (msg string, lineCount int) {
	if v.message == "" {
		return "", 1
	}

	var all []string
	for raw := range strings.SplitSeq(v.message, "\n") {
		all = append(all, wrapLines(raw, maxW)...)
	}
	if len(all) > maxLines {
		all = all[:maxLines]
	}
	return strings.Join(all, "\n"), len(all)
}
