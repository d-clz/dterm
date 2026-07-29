package ui

import (
	"fmt"
	"strings"
	"time"

	"dterm/internal/config"
	"dterm/internal/dlog"
	"dterm/internal/sshclient"
	"dterm/internal/vt"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// --- messages ---

type sshOutputMsg struct {
	paneID int
	data   []byte
}

type sshClosedMsg struct {
	paneID int
	err    error
}

type sshConnectedMsg struct {
	paneID  int
	session *sshclient.Session
}

// --- pane ---

type selPt struct{ x, y int }

type Pane struct {
	id               int
	bookmark         config.Bookmark
	session          *sshclient.Session
	term             *vt.Terminal
	width            int
	height           int
	connected        bool
	statusText       string
	showTS           bool
	lastActivity     time.Time
	awaitingPassword bool
	passwordInput    textinput.Model
	tempPassword     string
	selecting        bool
	hasSelection     bool
	selStart         selPt
	selEnd           selPt
}

func NewPane(id int, bm config.Bookmark) Pane {
	ti := textinput.New()
	ti.Placeholder = "password"
	ti.EchoMode = textinput.EchoPassword
	ti.EchoCharacter = '•'
	ti.Width = 32
	return Pane{
		id:            id,
		bookmark:      bm,
		term:          vt.New(0, 0), // real size set by first Resize call
		statusText:    "connecting...",
		passwordInput: ti,
	}
}

func (p *Pane) Connect() tea.Cmd {
	bm := p.bookmark
	if p.tempPassword != "" {
		bm.Password = p.tempPassword
	}
	id := p.id
	return func() (msg tea.Msg) {
		defer func() {
			if r := recover(); r != nil {
				dlog.Log("pane %d: PANIC in connect: %v", id, r)
				msg = sshClosedMsg{paneID: id, err: fmt.Errorf("panic: %v", r)}
			}
		}()
		sess, err := sshclient.Connect(bm)
		if err != nil {
			dlog.Log("pane %d: connect error: %v", id, err)
			return sshClosedMsg{paneID: id, err: err}
		}
		return sshConnectedMsg{paneID: id, session: sess}
	}
}

func (p *Pane) StartPasswordPrompt() {
	p.awaitingPassword = true
	p.statusText = "enter password"
	p.passwordInput.Reset()
	p.passwordInput.Focus()
}

func (p *Pane) SubmitPassword() tea.Cmd {
	p.tempPassword = p.passwordInput.Value()
	p.awaitingPassword = false
	p.passwordInput.Blur()
	p.statusText = "connecting..."
	return p.Connect()
}

func (p *Pane) CancelPassword() {
	p.awaitingPassword = false
	p.passwordInput.Blur()
	p.statusText = "cancelled"
}

// readLoop reads a batch of SSH output and returns it as a message.
// ReadBatch coalesces up to 2ms of data to reduce render frequency.
func readLoop(id int, sess *sshclient.Session) tea.Cmd {
	return func() (msg tea.Msg) {
		defer func() {
			if r := recover(); r != nil {
				dlog.Log("pane %d: PANIC in readLoop: %v", id, r)
				msg = sshClosedMsg{paneID: id, err: fmt.Errorf("panic: %v", r)}
			}
		}()
		data, err := sess.ReadBatch()
		if err != nil {
			return sshClosedMsg{paneID: id, err: err}
		}
		return sshOutputMsg{paneID: id, data: data}
	}
}

func (p *Pane) HandleOutput(data []byte) {
	atBottom := !p.term.IsScrolled()
	p.term.Write(data)
	if atBottom {
		p.term.ScrollToBottom()
	}
	p.lastActivity = time.Now()
}

func (p *Pane) ScrollUp(n int)    { p.term.ScrollUp(n) }
func (p *Pane) ScrollDown(n int)  { p.term.ScrollDown(n) }
func (p *Pane) ScrollToBottom()   { p.term.ScrollToBottom() }

func (p *Pane) ToggleTimestamp() {
	p.showTS = !p.showTS
	p.Resize(p.width, p.height) // recompute VT width with/without prefix
}

func (p *Pane) WriteInput(data []byte) {
	if p.session != nil {
		p.session.Write(data)
	}
}

const tsPrefixWidth = 11 // "[HH:MM:SS] "

// paneInner returns the usable (innerW, innerH) for a pane of outer size w×h,
// accounting for the 1-cell border on each side.
func paneInner(w, h int) (int, int) {
	iw := w - 2
	if iw < 1 {
		iw = 1
	}
	ih := h - 2
	if ih < 1 {
		ih = 1
	}
	return iw, ih
}

func (p *Pane) vtWidth(innerW int) int {
	if p.showTS {
		innerW -= tsPrefixWidth
	}
	if innerW < 1 {
		innerW = 1
	}
	return innerW
}

func (p *Pane) Resize(w, h int) {
	p.width = w
	p.height = h
	innerW, innerH := paneInner(w, h)
	vtW := p.vtWidth(innerW)
	p.term.Resize(vtW, innerH)
	if p.session != nil && p.connected {
		p.session.Resize(innerH, vtW)
	}
}

func (p *Pane) Close() {
	if p.session != nil {
		p.session.Close()
	}
}

func (p *Pane) StartSel(x, y int) {
	p.selecting = true
	p.hasSelection = false
	p.selStart = selPt{x, y}
	p.selEnd = selPt{x, y}
}

func (p *Pane) UpdateSel(x, y int) {
	if p.selecting {
		p.selEnd = selPt{x, y}
	}
}

func (p *Pane) EndSel(x, y int) string {
	if !p.selecting {
		return ""
	}
	p.selEnd = selPt{x, y}
	p.selecting = false
	if p.selStart == p.selEnd {
		p.hasSelection = false
		return ""
	}
	p.hasSelection = true
	return p.term.TextRange(p.selStart.x, p.selStart.y, p.selEnd.x, p.selEnd.y)
}

func (p *Pane) ClearSel() {
	p.selecting = false
	p.hasSelection = false
}

func (p *Pane) CopySelection() string {
	if !p.hasSelection {
		return ""
	}
	return p.term.TextRange(p.selStart.x, p.selStart.y, p.selEnd.x, p.selEnd.y)
}

func (p *Pane) SelectWord(x, y int) string {
	x1, x2 := p.term.WordBounds(x, y)
	p.selecting = false
	p.selStart = selPt{x1, y}
	p.selEnd = selPt{x2, y}
	p.hasSelection = x1 != x2
	if !p.hasSelection {
		return ""
	}
	return p.term.TextRange(x1, y, x2, y)
}

func (p *Pane) SelectLine(y int) string {
	cols := p.term.Cols()
	p.selecting = false
	p.selStart = selPt{0, y}
	p.selEnd = selPt{cols - 1, y}
	p.hasSelection = cols > 0
	return p.term.TextRange(0, y, cols-1, y)
}

func (p *Pane) View(active bool, tunnelBadge string) string {
	innerW, innerH := paneInner(p.width, p.height)

	body := stylePaneBody
	if active {
		body = stylePaneBodyActive
	}

	var content string
	if p.awaitingPassword {
		content = p.passwordView(innerW, innerH)
	} else if p.showTS {
		content = p.renderWithTimestamps(active)
	} else if p.selecting || p.hasSelection {
		content = p.term.RenderWithSel(p.selStart.x, p.selStart.y, p.selEnd.x, p.selEnd.y)
	} else if active {
		content = p.term.RenderWithCursor()
	} else {
		content = p.term.Render()
	}

	return p.topBorderLine(innerW, active, tunnelBadge) + "\n" +
		body.Width(innerW).Height(innerH).Render(content)
}

func (p *Pane) topBorderLine(innerW int, active bool, badge string) string {
	title := p.bookmark.User + "@" + p.bookmark.Host
	if !p.connected {
		title += " (" + p.statusText + ")"
	}
	if p.term.IsScrolled() {
		title += " [scroll]"
	}
	// badge is kept separate — it contains ANSI codes that must not be rune-counted

	var fg lipgloss.TerminalColor = subtle
	if active {
		fg = highlight
	}
	bar := lipgloss.NewStyle().Foreground(fg)

	total := innerW + 2
	if total < 5 {
		return bar.Render("╭" + strings.Repeat("─", max(0, total-2)) + "╮")
	}

	badgeW := lipgloss.Width(badge) // visual width, ANSI codes stripped
	titleWithSpaces := " " + title + " "
	titleRunes := []rune(titleWithSpaces)
	maxTitle := total - 4 - badgeW // ╭─{title}─{badge}╮ overhead
	if maxTitle < 0 {
		maxTitle = 0
	}
	if len(titleRunes) > maxTitle {
		titleRunes = titleRunes[:maxTitle]
		titleWithSpaces = string(titleRunes)
	}

	fill := total - len(titleRunes) - 3 - badgeW // ╭─ title ─{fill}{badge}╮
	if fill < 1 {
		fill = 1
	}

	return bar.Render("╭─") + titleWithSpaces + bar.Render(strings.Repeat("─", fill)) + badge + bar.Render("╮")
}

func (p *Pane) renderWithTimestamps(active bool) string {
	rows := p.term.Rows()
	hasSel := p.selecting || p.hasSelection
	x1, y1, x2, y2 := p.selStart.x, p.selStart.y, p.selEnd.x, p.selEnd.y
	var sb strings.Builder
	for y := 0; y < rows; y++ {
		ts := p.term.RowTime(y)
		if ts.IsZero() {
			sb.WriteString(strings.Repeat(" ", tsPrefixWidth))
		} else {
			sb.WriteString(styleTimestamp.Render(ts.Format("15:04:05")))
			sb.WriteByte(' ')
		}
		switch {
		case hasSel:
			sb.WriteString(p.term.RenderRowWithSel(y, x1, y1, x2, y2))
		case active:
			sb.WriteString(p.term.RenderRowWithCursor(y))
		default:
			sb.WriteString(p.term.RenderRow(y))
		}
		if y < rows-1 {
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

func (p *Pane) passwordView(w, h int) string {
	label := fmt.Sprintf("Password for %s@%s", p.bookmark.User, p.bookmark.Host)
	hint := "enter to connect • esc to cancel"
	prompt := lipgloss.JoinVertical(lipgloss.Center,
		styleBookmarkName.Render(label),
		p.passwordInput.View(),
		styleHelp.Render(hint),
	)
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, prompt)
}

