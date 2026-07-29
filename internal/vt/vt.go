// Package vt implements a minimal VT100/xterm terminal emulator with ANSI color support.
package vt

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// smallInts is a pre-built lookup for strconv.Itoa(0..255), eliminating
// per-call allocations in the SGR rendering hot path.
var smallInts [256]string

func init() {
	for i := range smallInts {
		smallInts[i] = strconv.Itoa(i)
	}
}

// ---- color / style --------------------------------------------------------

type ColorMode uint8

const (
	ColorDefault  ColorMode = iota
	ColorStandard           // 3/4-bit (0-7 standard, 8-15 bright)
	Color256                // 8-bit
	ColorRGB                // 24-bit true color
)

type Color struct {
	Mode    ColorMode
	Value   uint8 // standard or 256-color index
	R, G, B uint8 // RGB only
}

type Style struct {
	FG, BG    Color
	Bold      bool
	Dim       bool
	Italic    bool
	Underline bool
	Reverse   bool
	Strike    bool
}

// ---- cell -----------------------------------------------------------------

type Cell struct {
	Ch    rune
	Style Style
}

// ---- terminal -------------------------------------------------------------

const maxScrollback = 2000

type Terminal struct {
	cols, rows    int
	cells         [][]Cell
	rowTimes      []time.Time // last time each row was written to
	scrollback    [][]Cell
	scrollTimes   []time.Time
	scrollOffset  int // lines scrolled back from live; 0 = live view
	curX, curY    int
	savedX        int
	savedY        int
	curStyle      Style
	partial       []byte
	pendingWrap   bool // deferred wrap: set when cursor lands on last col
	scrollTop     int  // DECSTBM top margin (0-based, inclusive)
	scrollBottom  int  // DECSTBM bottom margin (0-based, inclusive)
	writeTime     time.Time // timestamp of the current Write() call, shared by putRune
	// Alternate screen (DECSET 1049 / 47)
	altActive    bool
	altCells     [][]Cell
	altTimes     []time.Time
	altCurX      int
	altCurY      int
	altSavedX    int
	altSavedY    int
}

// New creates a Terminal. Pass 0,0 when size is not yet known (Resize will be called before first render).
func New(cols, rows int) *Terminal {
	if cols < 1 {
		cols = 80
	}
	if rows < 1 {
		rows = 24
	}
	t := &Terminal{cols: cols, rows: rows, scrollBottom: rows - 1}
	t.allocCells()
	return t
}

func (t *Terminal) allocCells() {
	t.cells = make([][]Cell, t.rows)
	t.rowTimes = make([]time.Time, t.rows)
	for i := range t.cells {
		t.cells[i] = t.blankLine()
	}
}

func (t *Terminal) blankLine() []Cell { return t.newLine(t.cols) }

func (t *Terminal) newLine(cols int) []Cell {
	line := make([]Cell, cols)
	for i := range line {
		line[i] = Cell{Ch: ' '}
	}
	return line
}

func (t *Terminal) fillSpaces(line []Cell, from, to int) {
	if from < 0 {
		from = 0
	}
	if to > len(line) {
		to = len(line)
	}
	for i := from; i < to; i++ {
		line[i] = Cell{Ch: ' '}
	}
}

func (t *Terminal) Resize(cols, rows int) {
	if cols < 1 {
		cols = 1
	}
	if rows < 1 {
		rows = 1
	}
	oldCols := t.cols
	newCells := make([][]Cell, rows)
	newTimes := make([]time.Time, rows)
	for i := range newCells {
		newCells[i] = t.newLine(cols) // allocate at NEW cols
		if i < len(t.cells) {
			n := oldCols
			if cols < n {
				n = cols
			}
			copy(newCells[i], t.cells[i][:n])
			newTimes[i] = t.rowTimes[i]
		}
	}
	t.cells = newCells
	t.rowTimes = newTimes
	t.cols = cols
	t.rows = rows
	t.scrollTop = 0
	t.scrollBottom = rows - 1
	// Resize alt-screen cells if they exist.
	if t.altCells != nil {
		newAlt := make([][]Cell, rows)
		newAltTimes := make([]time.Time, rows)
		for i := range newAlt {
			newAlt[i] = t.newLine(cols)
			if i < len(t.altCells) {
				n := oldCols
				if cols < n {
					n = cols
				}
				copy(newAlt[i], t.altCells[i][:n])
				newAltTimes[i] = t.altTimes[i]
			}
		}
		t.altCells = newAlt
		t.altTimes = newAltTimes
	}
	// Resize scrollback rows so render loops don't panic on out-of-bounds x.
	for i, row := range t.scrollback {
		if len(row) == cols {
			continue
		}
		nr := t.newLine(cols)
		n := len(row)
		if n > cols {
			n = cols
		}
		copy(nr, row[:n])
		t.scrollback[i] = nr
	}
	t.clamp()
}

func (t *Terminal) clamp() {
	if t.curX < 0 {
		t.curX = 0
	}
	if t.curX >= t.cols {
		t.curX = t.cols - 1
	}
	if t.curY < 0 {
		t.curY = 0
	}
	if t.curY >= t.rows {
		t.curY = t.rows - 1
	}
	t.pendingWrap = false
}

// ---- input ----------------------------------------------------------------

func (t *Terminal) Write(p []byte) {
	t.writeTime = time.Now()
	var data []byte
	if len(t.partial) == 0 {
		data = p // avoid copy when no incomplete sequence is buffered
	} else {
		data = append(t.partial, p...)
	}
	t.partial = nil

	for len(data) > 0 {
		b := data[0]
		if b == '\x1b' {
			n := t.parseEscape(data)
			if n == 0 {
				t.partial = cloneBytes(data)
				return
			}
			data = data[n:]
			continue
		}
		if b >= 0x80 {
			r, size := utf8.DecodeRune(data)
			if r == utf8.RuneError && size == 1 {
				if utf8.FullRune(data) {
					// Genuinely invalid byte — emit replacement and advance.
					t.putRune(utf8.RuneError)
					data = data[1:]
				} else {
					// Incomplete multi-byte sequence — buffer and wait for more.
					t.partial = cloneBytes(data)
					return
				}
				continue
			}
			t.putRune(r)
			data = data[size:]
			continue
		}
		t.putRune(rune(b))
		data = data[1:]
	}
}

func (t *Terminal) putRune(r rune) {
	switch r {
	case '\r':
		t.curX = 0
		t.pendingWrap = false
	case '\n', '\f', '\v':
		t.pendingWrap = false
		t.linefeed()
	case '\b':
		t.pendingWrap = false
		if t.curX > 0 {
			t.curX--
		}
	case '\t':
		t.pendingWrap = false
		t.curX = (t.curX/8 + 1) * 8
		if t.curX >= t.cols {
			t.curX = t.cols - 1
		}
	case '\a', '\x0e', '\x0f':
		// bell, shift-out, shift-in — ignore
	default:
		if r < ' ' {
			return
		}
		// deferred wrap: if previous char landed on the last column, wrap now
		if t.pendingWrap {
			t.pendingWrap = false
			t.curX = 0
			t.linefeed()
		}
		if t.curY >= 0 && t.curY < t.rows && t.curX >= 0 && t.curX < t.cols {
			t.cells[t.curY][t.curX] = Cell{Ch: r, Style: t.curStyle}
			t.rowTimes[t.curY] = t.writeTime
		}
		t.curX++
		if t.curX >= t.cols {
			t.curX = t.cols - 1
			t.pendingWrap = true
		}
	}
}

func (t *Terminal) linefeed() {
	if t.curY == t.scrollBottom {
		t.scrollUp(1)
	} else {
		t.curY++
		if t.curY >= t.rows {
			t.curY = t.rows - 1
		}
	}
}

func (t *Terminal) scrollUp(n int) {
	top := t.scrollTop
	bot := t.scrollBottom
	if bot >= t.rows {
		bot = t.rows - 1
	}
	regionH := bot - top + 1
	if n > regionH {
		n = regionH
	}
	for i := 0; i < n; i++ {
		// Only save to scrollback when the full screen scrolls (region = whole screen).
		if top == 0 && bot == t.rows-1 && !t.altActive {
			saved := make([]Cell, len(t.cells[top]))
			copy(saved, t.cells[top])
			t.scrollback = append(t.scrollback, saved)
			t.scrollTimes = append(t.scrollTimes, t.rowTimes[top])
			if len(t.scrollback) > maxScrollback {
				t.scrollback = t.scrollback[1:]
				t.scrollTimes = t.scrollTimes[1:]
			}
		}
		copy(t.cells[top:bot], t.cells[top+1:bot+1])
		t.cells[bot] = t.blankLine()
		copy(t.rowTimes[top:bot], t.rowTimes[top+1:bot+1])
		t.rowTimes[bot] = time.Time{}
	}
}

func (t *Terminal) scrollDown(n int) {
	top := t.scrollTop
	bot := t.scrollBottom
	if bot >= t.rows {
		bot = t.rows - 1
	}
	regionH := bot - top + 1
	if n > regionH {
		n = regionH
	}
	for i := 0; i < n; i++ {
		copy(t.cells[top+1:bot+1], t.cells[top:bot])
		t.cells[top] = t.blankLine()
		copy(t.rowTimes[top+1:bot+1], t.rowTimes[top:bot])
		t.rowTimes[top] = time.Time{}
	}
}

// ---- escape parsers -------------------------------------------------------

func (t *Terminal) parseEscape(data []byte) int {
	if len(data) < 2 {
		return 0
	}
	switch data[1] {
	case '[':
		return t.parseCSI(data)
	case ']':
		return t.consumeToST(data, 2)
	case 'P', 'X', '^', '_':
		return t.consumeToST(data, 2)
	case '(':
		if len(data) < 3 {
			return 0
		}
		return 3
	case 'M':
		if t.curY == t.scrollTop {
			t.scrollDown(1)
		} else {
			t.curY--
			if t.curY < 0 {
				t.curY = 0
			}
		}
		return 2
	case '7':
		t.savedX, t.savedY = t.curX, t.curY
		return 2
	case '8':
		t.curX, t.curY = t.savedX, t.savedY
		t.clamp()
		return 2
	case 'c':
		t.curX, t.curY = 0, 0
		t.curStyle = Style{}
		t.scrollTop = 0
		t.scrollBottom = t.rows - 1
		t.allocCells()
		return 2
	case '=', '>', 'F', 'G', 'H', 'N', 'O':
		return 2
	}
	return 2
}

func (t *Terminal) consumeToST(data []byte, start int) int {
	for i := start; i < len(data); i++ {
		if data[i] == '\a' {
			return i + 1
		}
		if data[i] == '\x1b' && i+1 < len(data) && data[i+1] == '\\' {
			return i + 2
		}
	}
	return 0
}

func (t *Terminal) parseCSI(data []byte) int {
	i := 2
	for i < len(data) && data[i] >= 0x20 && data[i] <= 0x3f {
		i++
	}
	if i >= len(data) {
		return 0
	}
	final := data[i]
	if final < 0x40 || final > 0x7e {
		return i + 1
	}
	paramStr := string(data[2:i])
	t.handleCSI(final, paramStr)
	return i + 1
}

func (t *Terminal) handleCSI(final byte, paramStr string) {
	priv := len(paramStr) > 0 && paramStr[0] == '?'
	if priv || (len(paramStr) > 0 && paramStr[0] == '>') {
		paramStr = paramStr[1:]
	}
	ps := parseParams(paramStr)

	switch final {
	case 'H', 'f':
		t.curY = pdef(ps, 0, 1) - 1
		t.curX = pdef(ps, 1, 1) - 1
		t.clamp()
	case 'A':
		t.curY -= p1(ps, 0)
		if t.curY < 0 {
			t.curY = 0
		}
	case 'B', 'e':
		t.curY += p1(ps, 0)
		if t.curY >= t.rows {
			t.curY = t.rows - 1
		}
	case 'C', 'a':
		t.curX += p1(ps, 0)
		if t.curX >= t.cols {
			t.curX = t.cols - 1
		}
	case 'D':
		t.curX -= p1(ps, 0)
		if t.curX < 0 {
			t.curX = 0
		}
	case 'E':
		t.curY += p1(ps, 0)
		t.curX = 0
		if t.curY >= t.rows {
			t.curY = t.rows - 1
		}
	case 'F':
		t.curY -= p1(ps, 0)
		t.curX = 0
		if t.curY < 0 {
			t.curY = 0
		}
	case 'G', '`':
		t.curX = pdef(ps, 0, 1) - 1
		t.clamp()
	case 'd':
		t.curY = pdef(ps, 0, 1) - 1
		t.clamp()
	case 'J':
		switch pdef(ps, 0, 0) {
		case 0:
			t.fillSpaces(t.cells[t.curY], t.curX, t.cols)
			for y := t.curY + 1; y < t.rows; y++ {
				t.fillSpaces(t.cells[y], 0, t.cols)
			}
		case 1:
			t.fillSpaces(t.cells[t.curY], 0, t.curX+1)
			for y := 0; y < t.curY; y++ {
				t.fillSpaces(t.cells[y], 0, t.cols)
			}
		case 2, 3:
			for y := 0; y < t.rows; y++ {
				t.fillSpaces(t.cells[y], 0, t.cols)
			}
		}
	case 'K':
		switch pdef(ps, 0, 0) {
		case 0:
			t.fillSpaces(t.cells[t.curY], t.curX, t.cols)
		case 1:
			t.fillSpaces(t.cells[t.curY], 0, t.curX+1)
		case 2:
			t.fillSpaces(t.cells[t.curY], 0, t.cols)
		}
	case 'L':
		n := clampN(p1(ps, 0), t.rows)
		for i := 0; i < n; i++ {
			copy(t.cells[t.curY+1:t.rows], t.cells[t.curY:t.rows-1])
			t.cells[t.curY] = t.blankLine()
		}
	case 'M':
		n := clampN(p1(ps, 0), t.rows)
		for i := 0; i < n; i++ {
			copy(t.cells[t.curY:t.rows-1], t.cells[t.curY+1:t.rows])
			t.cells[t.rows-1] = t.blankLine()
		}
	case 'P':
		n := p1(ps, 0)
		row := t.cells[t.curY]
		if t.curX+n < t.cols {
			copy(row[t.curX:], row[t.curX+n:])
			t.fillSpaces(row, t.cols-n, t.cols)
		} else {
			t.fillSpaces(row, t.curX, t.cols)
		}
	case '@':
		n := p1(ps, 0)
		row := t.cells[t.curY]
		if t.curX+n < t.cols {
			copy(row[t.curX+n:], row[t.curX:t.cols-n])
			t.fillSpaces(row, t.curX, t.curX+n)
		} else {
			t.fillSpaces(row, t.curX, t.cols)
		}
	case 'X':
		t.fillSpaces(t.cells[t.curY], t.curX, t.curX+p1(ps, 0))
	case 'S':
		t.scrollUp(clampN(p1(ps, 0), t.rows))
	case 'T':
		t.scrollDown(clampN(p1(ps, 0), t.rows))
	case 's':
		t.savedX, t.savedY = t.curX, t.curY
	case 'u':
		t.curX, t.curY = t.savedX, t.savedY
		t.clamp()
	case 'r':
		// DECSTBM — set scroll region
		top := pdef(ps, 0, 1) - 1
		bot := pdef(ps, 1, t.rows) - 1
		if top < 0 {
			top = 0
		}
		if bot >= t.rows {
			bot = t.rows - 1
		}
		if top < bot {
			t.scrollTop = top
			t.scrollBottom = bot
			t.curX, t.curY = 0, 0
		}
	case 'h':
		t.setMode(ps, priv, true)
	case 'l':
		t.setMode(ps, priv, false)
	case 'm':
		t.parseSGR(ps)
	}
}

// ---- mode set/reset (h / l) -----------------------------------------------

func (t *Terminal) setMode(ps []int, priv, set bool) {
	if !priv {
		return // ANSI modes (IRM, LNM, etc.) not needed yet
	}
	for _, p := range ps {
		switch p {
		case 47: // alternate screen (basic, no cursor save)
			if set {
				t.enterAltScreen(false)
			} else {
				t.leaveAltScreen(false)
			}
		case 1049: // alternate screen with cursor save/restore
			if set {
				t.enterAltScreen(true)
			} else {
				t.leaveAltScreen(true)
			}
		case 25: // DECTCEM cursor visibility — ignore (no cursor blink state)
		}
	}
}

func (t *Terminal) enterAltScreen(saveCursor bool) {
	if t.altActive {
		return
	}
	// Persist normal cursor position into saved fields for restore.
	if saveCursor {
		t.savedX, t.savedY = t.curX, t.curY
	}
	// Initialise alt cells if first use, otherwise clear them.
	if t.altCells == nil {
		t.altCells = make([][]Cell, t.rows)
		t.altTimes = make([]time.Time, t.rows)
		for i := range t.altCells {
			t.altCells[i] = t.blankLine()
		}
	} else {
		for i := range t.altCells {
			t.altCells[i] = t.blankLine()
			t.altTimes[i] = time.Time{}
		}
	}
	// Swap.
	t.cells, t.altCells = t.altCells, t.cells
	t.rowTimes, t.altTimes = t.altTimes, t.rowTimes
	t.altCurX, t.altCurY = t.curX, t.curY
	t.curX, t.curY = 0, 0
	t.scrollTop = 0
	t.scrollBottom = t.rows - 1
	t.altActive = true
}

func (t *Terminal) leaveAltScreen(restoreCursor bool) {
	if !t.altActive {
		return
	}
	// Swap back.
	t.cells, t.altCells = t.altCells, t.cells
	t.rowTimes, t.altTimes = t.altTimes, t.rowTimes
	t.altActive = false
	t.scrollTop = 0
	t.scrollBottom = t.rows - 1
	if restoreCursor {
		t.curX, t.curY = t.savedX, t.savedY
		t.clamp()
	} else {
		t.curX, t.curY = t.altCurX, t.altCurY
		t.clamp()
	}
}

// ---- SGR (color / attribute) ----------------------------------------------

func (t *Terminal) parseSGR(ps []int) {
	if len(ps) == 0 {
		t.curStyle = Style{}
		return
	}
	i := 0
	for i < len(ps) {
		p := ps[i]
		switch {
		case p == 0:
			t.curStyle = Style{}
		case p == 1:
			t.curStyle.Bold = true
		case p == 2:
			t.curStyle.Dim = true
		case p == 3:
			t.curStyle.Italic = true
		case p == 4:
			t.curStyle.Underline = true
		case p == 7:
			t.curStyle.Reverse = true
		case p == 9:
			t.curStyle.Strike = true
		case p == 22:
			t.curStyle.Bold, t.curStyle.Dim = false, false
		case p == 23:
			t.curStyle.Italic = false
		case p == 24:
			t.curStyle.Underline = false
		case p == 27:
			t.curStyle.Reverse = false
		case p == 29:
			t.curStyle.Strike = false
		case p >= 30 && p <= 37:
			t.curStyle.FG = Color{Mode: ColorStandard, Value: uint8(p - 30)}
		case p == 38:
			if i+2 < len(ps) && ps[i+1] == 5 {
				t.curStyle.FG = Color{Mode: Color256, Value: uint8(ps[i+2])}
				i += 2
			} else if i+4 < len(ps) && ps[i+1] == 2 {
				t.curStyle.FG = Color{Mode: ColorRGB, R: uint8(ps[i+2]), G: uint8(ps[i+3]), B: uint8(ps[i+4])}
				i += 4
			}
		case p == 39:
			t.curStyle.FG = Color{}
		case p >= 40 && p <= 47:
			t.curStyle.BG = Color{Mode: ColorStandard, Value: uint8(p - 40)}
		case p == 48:
			if i+2 < len(ps) && ps[i+1] == 5 {
				t.curStyle.BG = Color{Mode: Color256, Value: uint8(ps[i+2])}
				i += 2
			} else if i+4 < len(ps) && ps[i+1] == 2 {
				t.curStyle.BG = Color{Mode: ColorRGB, R: uint8(ps[i+2]), G: uint8(ps[i+3]), B: uint8(ps[i+4])}
				i += 4
			}
		case p == 49:
			t.curStyle.BG = Color{}
		case p >= 90 && p <= 97:
			t.curStyle.FG = Color{Mode: ColorStandard, Value: uint8(p - 90 + 8)}
		case p >= 100 && p <= 107:
			t.curStyle.BG = Color{Mode: ColorStandard, Value: uint8(p - 100 + 8)}
		}
		i++
	}
}

// ---- rendering ------------------------------------------------------------

// renderRow renders display row y applying an optional per-cell style tweak.
// It is the single loop that all public Render* methods delegate to.
func (t *Terminal) renderRow(y int, tweak func(x, y int, s Style) Style) string {
	row, _ := t.getDisplayRow(y)
	var sb strings.Builder
	sb.Grow(t.cols * 6) // rough: avg 4 content bytes + ANSI overhead per cell
	var cur Style
	for x := 0; x < t.cols; x++ {
		var cell Cell
		if row != nil {
			cell = row[x]
		}
		s := cell.Style
		if tweak != nil {
			s = tweak(x, y, s)
		}
		if s != cur {
			writeSGR(&sb, s)
			cur = s
		}
		ch := cell.Ch
		if ch == 0 {
			ch = ' ' // cells are initialized to ' ', but zero is safe to guard
		}
		sb.WriteRune(ch)
	}
	if cur != (Style{}) {
		sb.WriteString("\x1b[0m")
	}
	return sb.String()
}

// renderScreen renders all rows joined with newlines.
func (t *Terminal) renderScreen(tweak func(x, y int, s Style) Style) string {
	var sb strings.Builder
	sb.Grow(t.rows * (t.cols*6 + 1))
	for y := 0; y < t.rows; y++ {
		if y > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(t.renderRow(y, tweak))
	}
	return sb.String()
}

// Render returns the terminal screen as a string with ANSI color codes.
func (t *Terminal) Render() string { return t.renderScreen(nil) }

// RenderRow renders a single display row with ANSI color codes.
func (t *Terminal) RenderRow(y int) string {
	if y < 0 || y >= t.rows {
		return ""
	}
	return t.renderRow(y, nil)
}

// RenderWithCursor renders the terminal with a block cursor at the current position.
// The cursor is only shown when not scrolled into the scrollback buffer.
func (t *Terminal) RenderWithCursor() string {
	if t.scrollOffset != 0 {
		return t.renderScreen(nil)
	}
	cx, cy := t.curX, t.curY
	return t.renderScreen(func(x, y int, s Style) Style {
		if x == cx && y == cy {
			s.Reverse = !s.Reverse
		}
		return s
	})
}

// RenderRowWithCursor renders a single display row with the cursor highlighted.
func (t *Terminal) RenderRowWithCursor(y int) string {
	if y < 0 || y >= t.rows {
		return ""
	}
	if t.scrollOffset != 0 {
		return t.renderRow(y, nil)
	}
	cx, cy := t.curX, t.curY
	return t.renderRow(y, func(x, ry int, s Style) Style {
		if x == cx && ry == cy {
			s.Reverse = !s.Reverse
		}
		return s
	})
}

// RenderWithSel renders the full terminal with the selected range highlighted.
func (t *Terminal) RenderWithSel(x1, y1, x2, y2 int) string {
	x1, y1, x2, y2 = normalizeSel(x1, y1, x2, y2)
	return t.renderScreen(func(x, y int, s Style) Style {
		if t.inSel(x, y, x1, y1, x2, y2) {
			s.Reverse = !s.Reverse
		}
		return s
	})
}

// RenderRowWithSel renders a single display row with the selected range highlighted.
func (t *Terminal) RenderRowWithSel(y, x1, y1, x2, y2 int) string {
	if y < 0 || y >= t.rows {
		return ""
	}
	x1, y1, x2, y2 = normalizeSel(x1, y1, x2, y2)
	return t.renderRow(y, func(x, ry int, s Style) Style {
		if t.inSel(x, ry, x1, y1, x2, y2) {
			s.Reverse = !s.Reverse
		}
		return s
	})
}

// writeSGR writes the ANSI SGR escape sequence for style to directly into sb,
// avoiding all intermediate string allocations compared to a string-returning approach.
func writeSGR(sb *strings.Builder, to Style) {
	if to == (Style{}) {
		sb.WriteString("\x1b[0m")
		return
	}
	sb.WriteString("\x1b[0")
	if to.Bold {
		sb.WriteString(";1")
	}
	if to.Dim {
		sb.WriteString(";2")
	}
	if to.Italic {
		sb.WriteString(";3")
	}
	if to.Underline {
		sb.WriteString(";4")
	}
	if to.Reverse {
		sb.WriteString(";7")
	}
	if to.Strike {
		sb.WriteString(";9")
	}
	writeColorSGR(sb, to.FG, true)
	writeColorSGR(sb, to.BG, false)
	sb.WriteByte('m')
}

// writeColorSGR writes the color portion of an SGR sequence to sb.
// Uses the pre-built smallInts table to avoid strconv.Itoa allocations.
func writeColorSGR(sb *strings.Builder, c Color, fg bool) {
	switch c.Mode {
	case ColorDefault:
		return
	case ColorStandard:
		base := 30
		if !fg {
			base = 40
		}
		sb.WriteByte(';')
		if c.Value >= 8 {
			sb.WriteString(smallInts[base+60+int(c.Value-8)])
		} else {
			sb.WriteString(smallInts[base+int(c.Value)])
		}
	case Color256:
		if fg {
			sb.WriteString(";38;5;")
		} else {
			sb.WriteString(";48;5;")
		}
		sb.WriteString(smallInts[c.Value])
	case ColorRGB:
		if fg {
			sb.WriteString(";38;2;")
		} else {
			sb.WriteString(";48;2;")
		}
		sb.WriteString(smallInts[c.R])
		sb.WriteByte(';')
		sb.WriteString(smallInts[c.G])
		sb.WriteByte(';')
		sb.WriteString(smallInts[c.B])
	}
}

// ---- helpers --------------------------------------------------------------

func parseParams(s string) []int {
	if s == "" {
		return nil
	}
	var out []int
	n := 0
	for _, ch := range s {
		if ch == ';' {
			out = append(out, n)
			n = 0
		} else if ch >= '0' && ch <= '9' {
			if n < 65535 {
				n = n*10 + int(ch-'0')
				if n > 65535 {
					n = 65535
				}
			}
		}
	}
	return append(out, n)
}

func clampN(n, max int) int {
	if n > max {
		return max
	}
	return n
}

func pdef(ps []int, idx, def int) int {
	if idx < len(ps) {
		return ps[idx]
	}
	return def
}

func p1(ps []int, idx int) int {
	if idx < len(ps) && ps[idx] > 0 {
		return ps[idx]
	}
	return 1
}

func (t *Terminal) Cols() int { return t.cols }
func (t *Terminal) Rows() int { return t.rows }

// WordBounds returns the x range [x1, x2] of the word at column x on display row y.
// A word is a contiguous run of non-space, non-null cells.
func (t *Terminal) WordBounds(x, y int) (x1, x2 int) {
	row, _ := t.getDisplayRow(y)
	if row == nil || len(row) == 0 {
		return x, x
	}
	if x < 0 {
		x = 0
	}
	if x >= len(row) {
		x = len(row) - 1
	}
	isWord := func(i int) bool {
		if i < 0 || i >= len(row) {
			return false
		}
		ch := row[i].Ch
		return ch != 0 && ch != ' '
	}
	if !isWord(x) {
		return x, x
	}
	x1, x2 = x, x
	for x1 > 0 && isWord(x1-1) {
		x1--
	}
	for x2 < len(row)-1 && isWord(x2+1) {
		x2++
	}
	return x1, x2
}

// ScrollUp scrolls the viewport up n lines into the scrollback buffer.
func (t *Terminal) ScrollUp(n int) {
	t.scrollOffset += n
	if t.scrollOffset > len(t.scrollback) {
		t.scrollOffset = len(t.scrollback)
	}
}

// ScrollDown scrolls the viewport down n lines toward live output.
func (t *Terminal) ScrollDown(n int) {
	t.scrollOffset -= n
	if t.scrollOffset < 0 {
		t.scrollOffset = 0
	}
}

// ScrollToBottom returns the viewport to the live terminal output.
func (t *Terminal) ScrollToBottom() { t.scrollOffset = 0 }

// IsScrolled reports whether the viewport is scrolled into the scrollback buffer.
func (t *Terminal) IsScrolled() bool { return t.scrollOffset > 0 }

// getDisplayRow returns the cell row and timestamp for display row y,
// taking the current scrollOffset into account.
func (t *Terminal) getDisplayRow(y int) ([]Cell, time.Time) {
	if y < 0 || y >= t.rows {
		return nil, time.Time{}
	}
	if t.scrollOffset == 0 {
		return t.cells[y], t.rowTimes[y]
	}
	sbLen := len(t.scrollback)
	start := sbLen - t.scrollOffset
	if start < 0 {
		start = 0
	}
	idx := start + y
	if idx < sbLen {
		return t.scrollback[idx], t.scrollTimes[idx]
	}
	liveIdx := idx - sbLen
	if liveIdx >= 0 && liveIdx < t.rows {
		return t.cells[liveIdx], t.rowTimes[liveIdx]
	}
	return nil, time.Time{}
}

// RowTime returns the time the display row y was last written to (zero if never).
func (t *Terminal) RowTime(y int) time.Time {
	_, ts := t.getDisplayRow(y)
	return ts
}

// TextRange extracts plain text from the terminal cells between two points.
// Coordinates are automatically normalized so order doesn't matter.
func (t *Terminal) TextRange(x1, y1, x2, y2 int) string {
	x1, y1, x2, y2 = normalizeSel(x1, y1, x2, y2)
	if y1 < 0 {
		y1 = 0
	}
	if y2 >= t.rows {
		y2 = t.rows - 1
	}
	var sb strings.Builder
	for y := y1; y <= y2; y++ {
		startX, endX := 0, t.cols-1
		if y == y1 {
			startX = clampCol(x1, t.cols)
		}
		if y == y2 {
			endX = clampCol(x2, t.cols)
		}
		displayRow, _ := t.getDisplayRow(y)
		row := make([]rune, 0, endX-startX+1)
		for x := startX; x <= endX; x++ {
			var ch rune
			if displayRow != nil && x < len(displayRow) {
				ch = displayRow[x].Ch
			}
			if ch == 0 {
				ch = ' '
			}
			row = append(row, ch)
		}
		// trim trailing spaces
		end := len(row)
		for end > 0 && row[end-1] == ' ' {
			end--
		}
		sb.WriteString(string(row[:end]))
		if y < y2 {
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

func normalizeSel(x1, y1, x2, y2 int) (int, int, int, int) {
	if y1 > y2 || (y1 == y2 && x1 > x2) {
		return x2, y2, x1, y1
	}
	return x1, y1, x2, y2
}

func (t *Terminal) inSel(x, y, x1, y1, x2, y2 int) bool {
	if y < y1 || y > y2 {
		return false
	}
	if y == y1 && y == y2 {
		return x >= x1 && x <= x2
	}
	if y == y1 {
		return x >= x1
	}
	if y == y2 {
		return x <= x2
	}
	return true
}

func clampCol(x, cols int) int {
	if x < 0 {
		return 0
	}
	if x >= cols {
		return cols - 1
	}
	return x
}

func cloneBytes(b []byte) []byte {
	c := make([]byte, len(b))
	copy(c, b)
	return c
}
