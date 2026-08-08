//go:build integration

package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// vtScreen reconstructs a terminal screen from the ANSI stream emitted by
// bubbletea's cursed renderer (charmbracelet/ultraviolet). The renderer is a
// cell-level diff writer: each frame only rewrites the cells that CHANGED,
// positioning them with CSI cursor-movement sequences. A naive ansi.Strip of
// the accumulated stream therefore cannot be asserted on — overwritten cells
// ("v" replacing "~" in a tool row) appear as isolated fragments and frames
// interleave. vtScreen replays the stream into a grid, exactly like a real
// terminal, so Screen() reflects what the user would actually see.
//
// The sequence set is bounded by what the UV renderer emits (verified
// against its source + captured output): CSI cursor moves (CUP/HPA/VPA/
// CUU/CUD/CUF/CUB/CNL/CPL), erase (EL/ED/ECH), scroll (DECSTBM/SU/SD/
// IL/DL), REP, SGR, mode sets (ignored), OSC (ignored), and the C0 controls
// \n \r \b \t. Everything else is ignored defensively.
type vtScreen struct {
	width, height  int
	grid           [][]vtCell
	curX, curY     int
	savedX, savedY int // CSI s/u and ESC 7/8
	// scroll region (inclusive rows). Default = full screen; DECSTBM (\x1b[r)
	// narrows it. Scroll ops (SU/SD/IL/DL) apply within the region only.
	top, bot int
}

// vtCell holds one terminal cell. text is the grapheme placed in this cell;
// width is its cell width (1 or 2 for wide CJK). Continuation cells of a
// wide grapheme hold text "" and width 0. A blank cell holds " " (the
// renderer erases by writing spaces).
type vtCell struct {
	text  string
	width int
}

func newVTScreen(width, height int) *vtScreen {
	s := &vtScreen{width: width, height: height}
	s.grid = make([][]vtCell, height)
	for y := range s.grid {
		s.grid[y] = make([]vtCell, width)
	}
	s.reset()
	return s
}

func (s *vtScreen) reset() {
	s.curX, s.curY = 0, 0
	s.savedX, s.savedY = 0, 0
	s.top, s.bot = 0, s.height-1
	s.clear()
}

// clear blanks the whole grid (cells become " ").
func (s *vtScreen) clear() {
	for y := 0; y < s.height; y++ {
		for x := 0; x < s.width; x++ {
			s.grid[y][x] = vtCell{text: " ", width: 1}
		}
	}
}

// feed parses and applies an ANSI stream chunk. A vtScreen is stateful and
// must be fed the accumulated output in order; feeding is not thread-safe.
func (s *vtScreen) feed(raw string) {
	p := ansi.NewParser()
	var state byte
	b := []byte(raw)
	for len(b) > 0 {
		seq, width, n, newState := ansi.DecodeSequence(b, state, p)
		b = b[n:]
		state = newState
		if width > 0 {
			s.write(string(seq), width)
			continue
		}
		s.handleSeq(seq, p)
	}
}

// handleSeq applies one control character or escape sequence.
func (s *vtScreen) handleSeq(seq []byte, p *ansi.Parser) {
	if len(seq) == 1 {
		switch seq[0] {
		case '\n': // LF: down one line; the renderer maps newline → col 0
			if s.curY < s.height-1 {
				s.curY++
			}
			s.curX = 0
		case '\r':
			s.curX = 0
		case '\b':
			if s.curX > 0 {
				s.curX--
			}
		case '\t':
			s.curX = min(s.curX+8-(s.curX%8), s.width-1)
		default: // BEL and other C0 controls: ignore
		}
		return
	}
	if seq[0] != 0x1b {
		return // stray non-ESC multi-byte sequence: ignore
	}
	switch seq[1] {
	case '[':
		s.handleCSI(p)
	case ']': // OSC (window title, cursor color, ...): ignore
	case '7': // DECSC save cursor
		s.savedX, s.savedY = s.curX, s.curY
	case '8': // DECRC restore cursor
		s.curX, s.curY = s.savedX, s.savedY
	case 'M': // RI (Reverse Index): cursor up one line, same column
		if s.curY > 0 {
			s.curY--
		} else {
			s.scrollDown(1)
		}
	case 'D': // IND (Index): cursor down one line, same column
		if s.curY < s.height-1 {
			s.curY++
		} else {
			s.scrollUp(1)
		}
	default: // other ESC sequences: ignore
	}
}

// handleCSI applies one CSI sequence using the parser's packed command and
// params.
func (s *vtScreen) handleCSI(p *ansi.Parser) {
	cmd := ansi.Cmd(p.Command())
	final := cmd.Final()
	prefix := cmd.Prefix()
	params := p.Params()

	if prefix != 0 {
		// Private sequences: mode sets (?25h/l, ?1049h/l, ?2004h/l, ...),
		// kitty keyboard (>=u, =u), DECRQSS (?u) — none affect the grid
		// except the alternate screen, which we treat as a full redraw
		// boundary: entering the alt screen starts a fresh buffer (the
		// renderer follows with \x1b[H\x1b[2J anyway), and exiting it we
		// KEEP the last alt-screen content — we never track the main
		// screen, and the last alt frame is what the user last saw.
		if prefix == '?' && final == 'h' && hasParam(params, 1049) {
			s.clear()
			s.curX, s.curY = 0, 0
		}
		return
	}

	param := func(i, def int) int {
		if i < len(params) {
			return params[i].Param(def)
		}
		return def
	}

	switch final {
	case 'H', 'f': // Cursor Position / HVP — params are ROW;COLUMN
		s.move(param(1, 1)-1, param(0, 1)-1)
	case 'A': // CUU
		s.move(s.curX, s.curY-param(0, 1))
	case 'B', 'e': // CUD / VPR
		s.move(s.curX, s.curY+param(0, 1))
	case 'C', 'a': // CUF / HPR
		s.move(s.curX+param(0, 1), s.curY)
	case 'D': // CUB
		s.move(s.curX-param(0, 1), s.curY)
	case 'E': // CNL: down n, col 0
		s.curY += param(0, 1)
		s.curX = 0
		s.clamp()
	case 'F': // CPL: up n, col 0
		s.curY -= param(0, 1)
		s.curX = 0
		s.clamp()
	case 'G', '`': // HPA / CHA
		s.curX = param(0, 1) - 1
		s.clamp()
	case 'd': // VPA
		s.curY = param(0, 1) - 1
		s.clamp()
	case 'K': // EL: 0/empty → to end, 1 → from start, 2 → whole line
		switch param(0, 0) {
		case 0:
			s.eraseLine(s.curX, s.width-1)
		case 1:
			s.eraseLine(0, s.curX)
		case 2:
			s.eraseLine(0, s.width-1)
		}
	case 'J': // ED: 0 → below, 1 → above, 2/3 → whole screen
		switch param(0, 0) {
		case 0:
			s.eraseLine(s.curX, s.width-1)
			for y := s.curY + 1; y < s.height; y++ {
				s.eraseLine(0, s.width-1)
			}
		case 1:
			s.eraseLine(0, s.curX)
			for y := 0; y < s.curY; y++ {
				s.eraseLine(0, s.width-1)
			}
		case 2, 3:
			s.clear()
		}
	case 'X': // ECH: erase n chars (fill with spaces), cursor stays
		s.eraseLine(s.curX, min(s.curX+param(0, 1)-1, s.width-1))
	case 'b': // REP: repeat previous grapheme n times
		if s.curX > 0 {
			prev := s.grid[s.curY][s.curX-1]
			if prev.text != "" && prev.text != " " {
				for i := 0; i < param(0, 1); i++ {
					s.write(prev.text, prev.width)
				}
			}
		}
	case 'P': // DCH: delete n chars, pull the rest of the line left
		n := param(0, 1)
		for x := s.curX; x < s.width; x++ {
			if x+n < s.width {
				s.grid[s.curY][x] = s.grid[s.curY][x+n]
			} else {
				s.grid[s.curY][x] = vtCell{text: " ", width: 1}
			}
		}
	case '@': // ICH: insert n blank chars, push the rest right
		n := param(0, 1)
		for x := s.width - 1; x >= s.curX; x-- {
			if x-n >= s.curX {
				s.grid[s.curY][x] = s.grid[s.curY][x-n]
			} else {
				s.grid[s.curY][x] = vtCell{text: " ", width: 1}
			}
		}
	case 'L': // IL: insert n blank lines at cursor row within the region
		s.insertLines(param(0, 1))
	case 'M': // DL: delete n lines at cursor row within the region
		s.deleteLines(param(0, 1))
	case 'S': // SU: scroll region up n lines
		s.scrollUp(param(0, 1))
	case 'T': // SD: scroll region down n lines
		s.scrollDown(param(0, 1))
	case 'r': // DECSTBM: set scroll region (top;bottom), cursor home
		s.top = param(0, 1) - 1
		s.bot = param(1, s.height) - 1
		s.top = max(s.top, 0)
		s.bot = min(s.bot, s.height-1)
		s.curX, s.curY = 0, 0
	case 's': // save cursor (ANSI.SYS)
		s.savedX, s.savedY = s.curX, s.curY
	case 'u': // restore cursor
		s.curX, s.curY = s.savedX, s.savedY
	case 'm', 'n', 'h', 'l', 'p', 'c', 'q': // SGR / DSR / mode sets / ...: ignore
	default:
		// Unknown commands (e.g. DCS-driven): ignore defensively.
	}
}

// write places a grapheme at the cursor and advances it. Wide graphemes
// occupy width cells; continuation cells are marked with text "".
func (s *vtScreen) write(g string, w int) {
	if w <= 0 {
		return
	}
	y := s.curY
	if y < 0 || y >= s.height {
		return
	}
	x := s.curX
	if x < 0 {
		x = 0
	}
	// Overwriting a cell that is a wide grapheme's continuation (or the
	// grapheme itself with a different width) clears the old grapheme.
	s.clearCovered(y, x, w)
	s.grid[y][x] = vtCell{text: g, width: w}
	for i := 1; i < w; i++ {
		if x+i < s.width {
			s.grid[y][x+i] = vtCell{text: "", width: 0}
		}
	}
	s.curX = x + w
	if s.curX >= s.width {
		// Wrap to the next line (terminal auto-wrap). The renderer avoids
		// writing past the last column, so this is defensive only.
		s.curX = 0
		s.curY++
	}
}

// clearCovered blanks the wide grapheme that occupies any of the cells
// [x, x+w).
func (s *vtScreen) clearCovered(y, x, w int) {
	for cx := max(x-1, 0); cx <= min(x+w, s.width-1); cx++ {
		c := s.grid[y][cx]
		if c.width > 1 {
			for i := 0; i < c.width && cx+i < s.width; i++ {
				s.grid[y][cx+i] = vtCell{text: " ", width: 1}
			}
			break
		}
	}
	// Cells we are about to write into get blanked regardless.
	for cx := x; cx < x+w && cx < s.width; cx++ {
		if s.grid[y][cx].width == 0 || s.grid[y][cx].text == "" {
			s.grid[y][cx] = vtCell{text: " ", width: 1}
		}
	}
}

func (s *vtScreen) eraseLine(from, to int) {
	from = max(from, 0)
	to = min(to, s.width-1)
	for x := from; x <= to; x++ {
		s.grid[s.curY][x] = vtCell{text: " ", width: 1}
	}
}

func (s *vtScreen) move(x, y int) {
	s.curX, s.curY = x, y
	s.clamp()
}

func (s *vtScreen) clamp() {
	s.curX = max(min(s.curX, s.width-1), 0)
	s.curY = max(min(s.curY, s.height-1), 0)
}

// scrollUp shifts the scroll region up n lines; blank lines appear at the
// bottom of the region.
func (s *vtScreen) scrollUp(n int) {
	if n <= 0 || s.top > s.bot {
		return
	}
	for y := s.top; y <= s.bot-n; y++ {
		s.grid[y] = s.grid[y+n]
	}
	for y := s.bot - n + 1; y <= s.bot; y++ {
		for x := range s.grid[y] {
			s.grid[y][x] = vtCell{text: " ", width: 1}
		}
	}
}

// scrollDown shifts the scroll region down n lines; blank lines appear at
// the top of the region.
func (s *vtScreen) scrollDown(n int) {
	if n <= 0 || s.top > s.bot {
		return
	}
	for y := s.bot; y >= s.top+n; y-- {
		s.grid[y] = s.grid[y-n]
	}
	for y := s.top; y < s.top+n; y++ {
		for x := range s.grid[y] {
			s.grid[y][x] = vtCell{text: " ", width: 1}
		}
	}
}

// insertLines inserts n blank lines at the cursor row inside the scroll
// region, pushing the rest down (lines beyond the region bottom are lost).
func (s *vtScreen) insertLines(n int) {
	if n <= 0 || s.curY < s.top || s.curY > s.bot {
		return
	}
	for y := s.bot; y >= s.curY+n; y-- {
		s.grid[y] = s.grid[y-n]
	}
	for y := s.curY; y < min(s.curY+n, s.bot+1); y++ {
		for x := range s.grid[y] {
			s.grid[y][x] = vtCell{text: " ", width: 1}
		}
	}
}

// deleteLines deletes n lines at the cursor row inside the scroll region,
// pulling the rest up (blank lines fill the bottom).
func (s *vtScreen) deleteLines(n int) {
	if n <= 0 || s.curY < s.top || s.curY > s.bot {
		return
	}
	for y := s.curY; y <= s.bot-n; y++ {
		s.grid[y] = s.grid[y+n]
	}
	for y := s.bot - n + 1; y <= s.bot; y++ {
		for x := range s.grid[y] {
			s.grid[y][x] = vtCell{text: " ", width: 1}
		}
	}
}

// Text returns the current screen as rows joined by newlines. Trailing
// spaces per row and fully blank trailing rows are trimmed so assertions
// match the visible content without terminal-width noise.
func (s *vtScreen) Text() string {
	var b strings.Builder
	for y := 0; y < s.height; y++ {
		var line strings.Builder
		for x := 0; x < s.width; x++ {
			line.WriteString(s.grid[y][x].text)
		}
		b.WriteString(strings.TrimRight(line.String(), " "))
		if y < s.height-1 {
			b.WriteByte('\n')
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// hasParam reports whether the given parameter value is present.
func hasParam(params ansi.Params, want int) bool {
	for _, p := range params {
		if p.Param(0) == want {
			return true
		}
	}
	return false
}
