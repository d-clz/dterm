package vt

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// ---- helpers ----------------------------------------------------------------

func cellText(t *Terminal) []string {
	rows := make([]string, t.rows)
	for y := 0; y < t.rows; y++ {
		var sb strings.Builder
		for x := 0; x < t.cols; x++ {
			ch := t.cells[y][x].Ch
			if ch == 0 {
				ch = ' '
			}
			sb.WriteRune(ch)
		}
		rows[y] = strings.TrimRight(sb.String(), " ")
	}
	return rows
}

func write(t *Terminal, s string) { t.Write([]byte(s)) }

// ---- basic text -------------------------------------------------------------

func TestPlainText(t *testing.T) {
	term := New(10, 3)
	write(term, "hello")
	rows := cellText(term)
	if rows[0] != "hello" {
		t.Errorf("row 0 = %q, want %q", rows[0], "hello")
	}
}

func TestNewline(t *testing.T) {
	term := New(10, 3)
	write(term, "ab\r\ncd")
	rows := cellText(term)
	if rows[0] != "ab" || rows[1] != "cd" {
		t.Errorf("rows = %v", rows)
	}
}

func TestWrap(t *testing.T) {
	term := New(5, 3)
	write(term, "abcdefgh") // wraps at col 5
	rows := cellText(term)
	if rows[0] != "abcde" || rows[1] != "fgh" {
		t.Errorf("rows = %v", rows)
	}
}

func TestScrollUp(t *testing.T) {
	term := New(5, 2)
	write(term, "line1\r\nline2\r\nline3")
	rows := cellText(term)
	if rows[0] != "line2" || rows[1] != "line3" {
		t.Errorf("scroll: rows = %v", rows)
	}
	if len(term.scrollback) != 1 {
		t.Errorf("scrollback len = %d, want 1", len(term.scrollback))
	}
}

// ---- UTF-8 ------------------------------------------------------------------

func TestUTF8Multibyte(t *testing.T) {
	term := New(10, 2)
	write(term, "café")
	rows := cellText(term)
	if !strings.HasPrefix(rows[0], "café") {
		t.Errorf("utf8: row 0 = %q", rows[0])
	}
}

func TestUTF8SplitAcrossWrites(t *testing.T) {
	// 'é' = 0xC3 0xA9 — split the two bytes across two writes
	term := New(10, 2)
	term.Write([]byte{'e', 0xC3}) // partial
	term.Write([]byte{0xA9})      // completes 'é'
	if term.cells[0][1].Ch != 'é' {
		t.Errorf("split utf8: cell[0][1] = %q, want 'é'", term.cells[0][1].Ch)
	}
}

func TestInvalidUTF8DoesNotStall(t *testing.T) {
	// A single 0xFF byte must emit U+FFFD and advance, not loop forever.
	term := New(10, 2)
	term.Write([]byte{0xFF, 'A'})
	// First cell should be replacement char, second should be 'A'.
	if term.cells[0][0].Ch != utf8.RuneError {
		t.Errorf("replacement: cell[0][0] = %q, want RuneError", term.cells[0][0].Ch)
	}
	if term.cells[0][1].Ch != 'A' {
		t.Errorf("after invalid byte: cell[0][1] = %q, want 'A'", term.cells[0][1].Ch)
	}
	// partial must be empty — no stall.
	if len(term.partial) != 0 {
		t.Errorf("partial not drained after invalid byte: %v", term.partial)
	}
}

func TestInvalidUTF8MultipleBadBytes(t *testing.T) {
	term := New(10, 2)
	term.Write([]byte{0xFF, 0xFF, 0xFF, 'Z'})
	if len(term.partial) != 0 {
		t.Errorf("partial not empty after 3 invalid bytes: %v", term.partial)
	}
	if term.cells[0][3].Ch != 'Z' {
		t.Errorf("cell[0][3] = %q, want 'Z'", term.cells[0][3].Ch)
	}
}

// ---- cursor movement --------------------------------------------------------

func TestCursorMove(t *testing.T) {
	term := New(10, 5)
	write(term, "\x1b[3;5H") // row 3, col 5 (1-based → 2, 4)
	if term.curY != 2 || term.curX != 4 {
		t.Errorf("cursor = (%d,%d), want (4,2)", term.curX, term.curY)
	}
}

func TestCursorUp(t *testing.T) {
	term := New(10, 5)
	write(term, "\x1b[3;1H\x1b[2A") // go to row 3, up 2
	if term.curY != 0 {
		t.Errorf("curY = %d, want 0", term.curY)
	}
}

func TestCursorClamped(t *testing.T) {
	term := New(5, 3)
	write(term, "\x1b[99;99H")
	if term.curX != 4 || term.curY != 2 {
		t.Errorf("clamped cursor = (%d,%d), want (4,2)", term.curX, term.curY)
	}
}

// ---- erase ------------------------------------------------------------------

func TestEraseToEOL(t *testing.T) {
	term := New(10, 3)
	write(term, "hello\x1b[1;3H\x1b[K") // write "hello", go to col 3, erase to EOL
	rows := cellText(term)
	if rows[0] != "he" {
		t.Errorf("after erase EOL: row 0 = %q", rows[0])
	}
}

func TestEraseScreen(t *testing.T) {
	term := New(5, 2)
	write(term, "hello\r\nworld\x1b[2J")
	rows := cellText(term)
	for i, r := range rows {
		if r != "" {
			t.Errorf("after ED2: row %d = %q, want empty", i, r)
		}
	}
}

// ---- CSI repeat-count clamp (#7) --------------------------------------------

func TestLargeInsertLineClamped(t *testing.T) {
	term := New(5, 3)
	write(term, "hello\r\nworld")
	// ESC[999999999L — must not spin or panic
	term.Write([]byte("\x1b[999999999L"))
	// Just verify we're still alive and terminal is sane.
	if term.curY < 0 || term.curY >= term.rows {
		t.Errorf("curY out of range after large IL: %d", term.curY)
	}
}

func TestParseParamsOverflow(t *testing.T) {
	ps := parseParams("99999999999")
	if ps[0] > 65535 {
		t.Errorf("parseParams overflow: %d", ps[0])
	}
}

// ---- scroll region (DECSTBM #8) --------------------------------------------

func TestScrollRegion(t *testing.T) {
	term := New(5, 5)
	// Fill rows 0-4 with labels.
	write(term, "row0\r\nrow1\r\nrow2\r\nrow3\r\nrow4")
	// Set scroll region to rows 2-4 (1-based: 3;5).
	write(term, "\x1b[3;5r")
	if term.scrollTop != 2 || term.scrollBottom != 4 {
		t.Errorf("scroll region = [%d,%d], want [2,4]", term.scrollTop, term.scrollBottom)
	}
	// Cursor should be at (0,0) after DECSTBM.
	if term.curX != 0 || term.curY != 0 {
		t.Errorf("cursor after DECSTBM = (%d,%d), want (0,0)", term.curX, term.curY)
	}
	// Move into region and trigger a linefeed scroll.
	write(term, "\x1b[5;1Hscrolled\r\n")
	rows := cellText(term)
	// row0 and row1 must be untouched.
	if !strings.HasPrefix(rows[0], "row0") {
		t.Errorf("row0 corrupted: %q", rows[0])
	}
	if !strings.HasPrefix(rows[1], "row1") {
		t.Errorf("row1 corrupted: %q", rows[1])
	}
}

func TestScrollRegionReverseIndex(t *testing.T) {
	// ESC M at top of scroll region should scroll down within region only.
	term := New(5, 4)
	write(term, "row0\r\nrow1\r\nrow2\r\nrow3")
	write(term, "\x1b[2;4r")    // region rows 2-4 (0-based 1-3)
	write(term, "\x1b[2;1H")    // cursor to row 2 (top of region)
	write(term, "\x1bM")        // reverse index — should insert blank at top of region
	rows := cellText(term)
	if !strings.HasPrefix(rows[0], "row0") {
		t.Errorf("row0 corrupted by RI: %q", rows[0])
	}
}

// ---- alternate screen (#8) --------------------------------------------------

func TestAltScreen(t *testing.T) {
	term := New(10, 3)
	write(term, "normal")
	// Enter alt screen (DECSET 1049).
	write(term, "\x1b[?1049h")
	if !term.altActive {
		t.Fatal("alt screen not active after DECSET 1049")
	}
	rows := cellText(term)
	for i, r := range rows {
		if r != "" {
			t.Errorf("alt screen not blank on entry: row %d = %q", i, r)
		}
	}
	write(term, "alt-content")
	// Leave alt screen.
	write(term, "\x1b[?1049l")
	if term.altActive {
		t.Fatal("alt screen still active after DECRST 1049")
	}
	rows = cellText(term)
	if !strings.HasPrefix(rows[0], "normal") {
		t.Errorf("normal screen not restored: row 0 = %q", rows[0])
	}
}

func TestAltScreenCursorRestore(t *testing.T) {
	term := New(10, 5)
	write(term, "\x1b[3;4H") // position cursor at row 3, col 4 on normal screen
	write(term, "\x1b[?1049h")
	write(term, "\x1b[?1049l")
	// Cursor should be restored to (row=2, col=3) (0-based).
	if term.curX != 3 || term.curY != 2 {
		t.Errorf("cursor after alt-screen restore = (%d,%d), want (3,2)", term.curX, term.curY)
	}
}

// ---- SGR --------------------------------------------------------------------

func TestSGRBold(t *testing.T) {
	term := New(10, 2)
	write(term, "\x1b[1mX\x1b[0mY")
	if !term.cells[0][0].Style.Bold {
		t.Error("cell 0 should be bold")
	}
	if term.cells[0][1].Style.Bold {
		t.Error("cell 1 should not be bold after reset")
	}
}

func TestSGR256Color(t *testing.T) {
	term := New(10, 2)
	write(term, "\x1b[38;5;200mX")
	c := term.cells[0][0].Style.FG
	if c.Mode != Color256 || c.Value != 200 {
		t.Errorf("256-color FG = %+v, want Mode=Color256 Value=200", c)
	}
}

// ---- resize -----------------------------------------------------------------

func TestResizePreservesContent(t *testing.T) {
	term := New(10, 3)
	write(term, "hello")
	term.Resize(20, 4)
	rows := cellText(term)
	if !strings.HasPrefix(rows[0], "hello") {
		t.Errorf("content lost after widen: row 0 = %q", rows[0])
	}
}

func TestResizeScrollbackNoPanic(t *testing.T) {
	// Reproduce the earlier scrollback-widen panic.
	term := New(5, 2)
	write(term, "line1\r\nline2\r\nline3") // pushes line1 to scrollback
	term.Resize(20, 2)                     // widen — must not panic
	term.ScrollUp(1)
	_ = term.Render() // must not panic
}
