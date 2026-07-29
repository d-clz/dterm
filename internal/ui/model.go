package ui

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"time"

	"dterm/internal/config"

	"github.com/atotto/clipboard"
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type viewState int

const (
	viewBookmarks viewState = iota
	viewTerminal
)

type splitDir int

const (
	splitNone splitDir = iota
	splitVertical
	splitHorizontal
)

type prefixKey int

const (
	prefixNone  prefixKey = iota
	prefixCtrlB // tmux-style prefix
)

type clickTrack struct {
	t    time.Time
	x, y int
	pane int
	n    int // consecutive click count: 1=single, 2=double, 3=triple
}

// tileNode is a node in the binary split tree.
// Leaf nodes (dir == splitNone) own a pane; interior nodes own a split.
type tileNode struct {
	dir   splitDir
	pane  int       // valid only when dir == splitNone
	left  *tileNode // left / top child
	right *tileNode // right / bottom child
}

type Model struct {
	cfg         *config.Config
	bookmarks   BookmarkView
	panes       []Pane
	root        *tileNode // layout tree
	active      int
	pendingSplit splitDir // direction for the next pane to open
	state       viewState
	prefix      prefixKey
	width       int
	height      int
	err         error

	sftp        *sftpModal
	sftpClients []*sftpClientHandle // keeps sftp clients alive until app exits
	transfers   []Transfer
	xferQueue   []xferJob

	tunnels      []Tunnel
	tunnelModal  *tunnelModal

	syncInput bool // when true, keyboard input is broadcast to all panes

	sftpLastLocal  string
	sftpLastRemote map[int]string
	lastClick      clickTrack

	paneIDSeq   int
	tunnelIDSeq int
}

// sftpClientHandle wraps an sftp.Client for deferred close.
type sftpClientHandle struct {
	client interface{ Close() error }
}

func NewModel(cfg *config.Config) Model {
	return Model{
		cfg:       cfg,
		bookmarks: NewBookmarkView(cfg.Bookmarks),
		panes:     []Pane{},
		state:     viewBookmarks,
	}
}

func (m Model) Init() tea.Cmd {
	return tea.SetWindowTitle("dterm")
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.bookmarks.Resize(msg.Width, msg.Height)
		m.resizePanes()
		return m, nil

	case tea.MouseMsg:
		return m.handleMouse(msg)

	case tea.KeyMsg:
		return m.handleKey(msg)

	case sshConnectedMsg:
		for i := range m.panes {
			if m.panes[i].id == msg.paneID {
				m.panes[i].session = msg.session
				m.panes[i].connected = true
				m.panes[i].statusText = "connected"
				m.panes[i].tempPassword = ""
				m.resizePanes()
				// Register saved tunnels as stopped — user toggles them on manually
				for _, spec := range m.panes[i].bookmark.Tunnels {
					m.tunnelIDSeq++
					id := m.tunnelIDSeq
					m.tunnels = append(m.tunnels, Tunnel{
						id: id, paneID: msg.paneID, spec: spec,
						status: tunnelStopped, saved: true,
					})
				}
				return m, readLoop(msg.paneID, msg.session)
			}
		}
		return m, nil

	case sshOutputMsg:
		for i := range m.panes {
			if m.panes[i].id == msg.paneID {
				m.panes[i].HandleOutput(msg.data)
				if m.panes[i].session != nil {
					return m, readLoop(msg.paneID, m.panes[i].session)
				}
			}
		}
		return m, nil

	case sshClosedMsg:
		for i := range m.panes {
			if m.panes[i].id == msg.paneID {
				if m.panes[i].connected {
					// session ended (e.g. user typed exit) → close pane without stealing focus
					return m.closePaneAt(i)
				}
				// connection never established → show error or password prompt
				if msg.err != nil && strings.Contains(msg.err.Error(), "no auth method") {
					m.panes[i].StartPasswordPrompt()
				} else {
					errText := "connection failed"
					if msg.err != nil {
						errText = msg.err.Error()
					}
					m.panes[i].statusText = errText
				}
			}
		}
		return m, nil

	// ---- SFTP ---------------------------------------------------------------

	case sftpOpenedMsg:
		if msg.err != nil {
			if m.active < len(m.panes) {
				m.panes[m.active].statusText = "sftp: " + msg.err.Error()
			}
			return m, nil
		}
		m.sftpClients = append(m.sftpClients, &sftpClientHandle{client: msg.client})
		modal, cmd := newSftpModal(msg.paneID, msg.client)
		m.sftp = modal
		return m, cmd

	case sftpDirLoadedMsg, sftpScanDoneMsg, sftpScanErrMsg, sftpCompletionsMsg:
		if m.sftp != nil {
			cmd, close := m.sftp.update(msg)
			if close {
				m.saveSftpDirs()
				m.sftp = nil
			}
			return m, cmd
		}
		return m, nil

	case sftpConfirmedMsg:
		var cmds []tea.Cmd
		for _, job := range msg.jobs {
			m.transfers = append(m.transfers, Transfer{
				id:     job.id,
				label:  labelFor(job),
				dir:    job.dir,
				total:  job.size,
				status: xferActive,
			})
			m.xferQueue = append(m.xferQueue, job)
		}
		if cmd := m.popAndStartTransfer(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)

	case sftpProgressMsg:
		for i := range m.transfers {
			if m.transfers[i].id == msg.id {
				m.transfers[i].done = msg.done
				m.transfers[i].total = msg.total
				break
			}
		}
		return m, msg.next

	case sftpXferDoneMsg:
		for i := range m.transfers {
			if m.transfers[i].id == msg.id {
				if msg.err != nil {
					m.transfers[i].status = xferFailed
					m.transfers[i].err = msg.err
				} else {
					m.transfers[i].status = xferDone
				}
				break
			}
		}
		id := msg.id
		return m, tea.Batch(
			m.popAndStartTransfer(),
			tea.Tick(3*time.Second, func(_ time.Time) tea.Msg {
				return sftpClearMsg{id: id}
			}),
		)

	case sftpClearMsg:
		for i, t := range m.transfers {
			if t.id == msg.id && t.status != xferActive {
				m.transfers = append(m.transfers[:i], m.transfers[i+1:]...)
				break
			}
		}
		return m, nil

	// ---- tunnels ------------------------------------------------------------

	case tunnelReadyMsg:
		for i := range m.tunnels {
			if m.tunnels[i].id == msg.id {
				m.tunnels[i].status = tunnelActive
				m.tunnels[i].listener = msg.listener
				break
			}
		}
		return m, msg.next

	case tunnelAcceptMsg:
		return m, msg.next

	case tunnelErrorMsg:
		for i := range m.tunnels {
			if m.tunnels[i].id == msg.id {
				m.tunnels[i].status = tunnelError
				m.tunnels[i].errMsg = msg.err.Error()
				if m.tunnels[i].listener != nil {
					m.tunnels[i].listener.Close()
					m.tunnels[i].listener = nil
				}
				break
			}
		}
		return m, nil

	case tunnelStartRequestMsg:
		for _, p := range m.panes {
			if p.id == msg.paneID && p.session != nil {
				m.tunnelIDSeq++
				id := m.tunnelIDSeq
				m.tunnels = append(m.tunnels, Tunnel{
					id: id, paneID: msg.paneID, spec: msg.spec,
					status: tunnelStarting,
				})
				return m, startTunnelCmd(id, msg.spec, p.session.Client())
			}
		}
		return m, nil

	case tunnelCloseRequestMsg:
		for i := range m.tunnels {
			if m.tunnels[i].id == msg.id {
				if m.tunnels[i].listener != nil {
					m.tunnels[i].listener.Close()
					m.tunnels[i].listener = nil
				}
				m.tunnels[i].status = tunnelStopped
				m.tunnels[i].errMsg = ""
				break
			}
		}
		return m, nil

	case tunnelDeleteRequestMsg:
		for i, t := range m.tunnels {
			if t.id == msg.id {
				if t.listener != nil {
					t.listener.Close()
				}
				m.tunnels = append(m.tunnels[:i], m.tunnels[i+1:]...)
				if m.tunnelModal != nil && m.tunnelModal.cursor >= len(m.tunnels) {
					m.tunnelModal.cursor = max(0, len(m.tunnels)-1)
				}
				break
			}
		}
		return m, nil

	case tunnelSaveRequestMsg:
		for i := range m.tunnels {
			if m.tunnels[i].id == msg.id {
				m.tunnels[i].saved = true
				m.saveTunnelToConfig(m.tunnels[i])
				break
			}
		}
		return m, nil

	case tunnelRetryRequestMsg:
		for i := range m.tunnels {
			if m.tunnels[i].id == msg.id {
				t := m.tunnels[i]
				for _, p := range m.panes {
					if p.id == t.paneID && p.session != nil {
						m.tunnelIDSeq++
						newID := m.tunnelIDSeq
						m.tunnels[i].id = newID
						m.tunnels[i].status = tunnelStarting
						m.tunnels[i].errMsg = ""
						m.tunnels[i].listener = nil
						return m, startTunnelCmd(newID, t.spec, p.session.Client())
					}
				}
				break
			}
		}
		return m, nil

	case tunnelEditRequestMsg:
		for i := range m.tunnels {
			if m.tunnels[i].id == msg.id {
				t := m.tunnels[i]
				if t.listener != nil {
					t.listener.Close()
				}
				wasRunning := t.status == tunnelActive || t.status == tunnelStarting
				m.tunnels[i].spec = msg.spec
				m.tunnels[i].status = tunnelStopped
				m.tunnels[i].errMsg = ""
				m.tunnels[i].listener = nil
				if t.saved {
					// Auto-update config when editing a saved tunnel
					m.tunnels[i].saved = true
					m.saveTunnelToConfig(m.tunnels[i])
				}
				if wasRunning {
					for _, p := range m.panes {
						if p.id == t.paneID && p.session != nil {
							m.tunnelIDSeq++
							newID := m.tunnelIDSeq
							m.tunnels[i].id = newID
							m.tunnels[i].status = tunnelStarting
							return m, startTunnelCmd(newID, msg.spec, p.session.Client())
						}
					}
				}
				break
			}
		}
		return m, nil
	}

	return m, nil
}

// ---- key routing ----------------------------------------------------------

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+q"))) {
		m.closeAll()
		return m, tea.Quit
	}
	// Tunnel modal swallows all keys except Ctrl+Q
	if m.tunnelModal != nil {
		cmd, close := m.tunnelModal.update(msg, m.tunnels)
		if close {
			m.tunnelModal = nil
		}
		return m, cmd
	}
	// SFTP modal swallows all keys except Ctrl+Q
	if m.sftp != nil {
		cmd, close := m.sftp.update(msg)
		if close {
			m.saveSftpDirs()
			m.sftp = nil
		}
		return m, cmd
	}
	switch m.state {
	case viewBookmarks:
		return m.handleBookmarkKey(msg)
	case viewTerminal:
		return m.handleTerminalKey(msg)
	}
	return m, nil
}

func (m Model) handleBookmarkKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	bv := &m.bookmarks
	if bv.searching {
		switch msg.String() {
		case "esc":
			bv.CancelSearch()
		case "enter":
			bv.ConfirmSearch()
			// connect immediately if exactly one result
			if len(bv.visible) == 1 {
				bm := bv.Selected()
				if bm != nil {
					return m.openPane(*bm)
				}
			}
		case "backspace", "ctrl+h":
			bv.BackspaceSearch()
		default:
			if len(msg.Runes) == 1 {
				bv.AppendSearch(msg.Runes[0])
			}
		}
		return m, nil
	}

	switch msg.String() {
	case "/":
		bv.StartSearch()
	case "esc":
		if bv.filter != "" {
			bv.CancelSearch()
		}
	case "up", "k":
		bv.Up()
	case "down", "j":
		bv.Down()
	case "h", "left":
		if bv.viewMode == viewModeGroup {
			bv.PrevGroup()
		}
	case "l", "right":
		if bv.viewMode == viewModeGroup {
			bv.NextGroup()
		}
	case "tab":
		if bv.viewMode == viewModeGroup {
			bv.NextGroup()
		}
	case "shift+tab":
		if bv.viewMode == viewModeGroup {
			bv.PrevGroup()
		}
	case "g":
		bv.ToggleViewMode()
	case "enter":
		bm := bv.Selected()
		if bm != nil {
			return m.openPane(*bm)
		}
	case "q", "ctrl+c":
		m.closeAll()
		return m, tea.Quit
	}
	return m, nil
}

func (m Model) handleTerminalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// 1. Password prompt swallows everything
	if m.active < len(m.panes) && m.panes[m.active].awaitingPassword {
		switch msg.String() {
		case "enter":
			cmd := m.panes[m.active].SubmitPassword()
			return m, cmd
		case "esc":
			m.panes[m.active].CancelPassword()
		default:
			var cmd tea.Cmd
			m.panes[m.active].passwordInput, cmd = m.panes[m.active].passwordInput.Update(msg)
			return m, cmd
		}
		return m, nil
	}

	// 2. Second key of a prefix chord
	if m.prefix != prefixNone {
		return m.handleChord(msg)
	}

	// 3. Check for prefix key
	if msg.String() == "ctrl+b" {
		m.prefix = prefixCtrlB
		return m, nil
	}

	// 4. Forward everything else to SSH
	if m.active < len(m.panes) {
		m.panes[m.active].ClearSel()
		m.panes[m.active].ScrollToBottom()
		data := keyBytes(msg)
		if m.syncInput {
			for i := range m.panes {
				m.panes[i].WriteInput(data)
			}
		} else {
			m.panes[m.active].WriteInput(data)
		}
	}
	return m, nil
}

// handleChord executes the action for the second key after the Ctrl+B prefix.
//
//	%        vertical split
//	"        horizontal split
//	h / ←   focus previous pane
//	l / →   focus next pane
//	k / ↑   focus previous pane
//	j / ↓   focus next pane
//	o / n   cycle to next pane
//	x        close active pane
//	b / spc  back to bookmark list
//	t        toggle per-line timestamps
//	s        toggle sync (broadcast input to all panes)
//	q        quit dterm
//	ctrl+b   send literal ^B to SSH
func (m Model) handleChord(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.prefix = prefixNone
	switch msg.String() {
	case "h", "left":
		m.active = m.navigateDir("left")
	case "l", "right":
		m.active = m.navigateDir("right")
	case "k", "up":
		m.active = m.navigateDir("up")
	case "j", "down":
		m.active = m.navigateDir("down")
	case "%":
		m.pendingSplit = splitVertical
		m.state = viewBookmarks
	case `"`:
		m.pendingSplit = splitHorizontal
		m.state = viewBookmarks
	case "o", "n":
		if len(m.panes) > 1 {
			m.active = (m.active + 1) % len(m.panes)
		}
	case "x":
		return m.closeActivePane()
	case "b", " ":
		m.state = viewBookmarks
	case "t":
		if m.active < len(m.panes) {
			m.panes[m.active].ToggleTimestamp()
		}
	case "s":
		m.syncInput = !m.syncInput
	case "f":
		if m.active < len(m.panes) && m.panes[m.active].session != nil {
			return m, openSftpCmd(m.active, m.panes[m.active].session.Client())
		}
	case "T":
		if m.active < len(m.panes) {
			p := m.panes[m.active]
			label := p.bookmark.User + "@" + p.bookmark.Host
			m.tunnelModal = newTunnelModal(p.id, label)
		}
	case "q":
		m.closeAll()
		return m, tea.Quit
	case "ctrl+b": // double-press → send literal ^B to SSH
		if m.active < len(m.panes) {
			m.panes[m.active].WriteInput([]byte("\x02"))
		}
	}
	return m, nil
}

// ---- key translation ------------------------------------------------------

// keyBytes translates a bubbletea KeyMsg into the byte sequence a VT100
// terminal expects to send to the host.
func keyBytes(msg tea.KeyMsg) []byte {
	switch msg.Type {
	case tea.KeyRunes:
		b := []byte(string(msg.Runes))
		if msg.Alt {
			return append([]byte{'\x1b'}, b...)
		}
		return b
	case tea.KeySpace:
		return []byte(" ")
	case tea.KeyEnter:
		return []byte("\r")
	case tea.KeyBackspace:
		return []byte("\x7f")
	case tea.KeyDelete:
		return []byte("\x1b[3~")
	case tea.KeyTab:
		return []byte("\t")
	case tea.KeyShiftTab:
		return []byte("\x1b[Z")
	case tea.KeyUp:
		if msg.Alt {
			return []byte("\x1b[1;3A")
		}
		return []byte("\x1b[A")
	case tea.KeyDown:
		if msg.Alt {
			return []byte("\x1b[1;3B")
		}
		return []byte("\x1b[B")
	case tea.KeyRight:
		if msg.Alt {
			return []byte("\x1b[1;3C")
		}
		return []byte("\x1b[C")
	case tea.KeyLeft:
		if msg.Alt {
			return []byte("\x1b[1;3D")
		}
		return []byte("\x1b[D")
	case tea.KeyHome:
		return []byte("\x1b[H")
	case tea.KeyEnd:
		return []byte("\x1b[F")
	case tea.KeyPgUp:
		return []byte("\x1b[5~")
	case tea.KeyPgDown:
		return []byte("\x1b[6~")
	case tea.KeyF1:
		return []byte("\x1bOP")
	case tea.KeyF2:
		return []byte("\x1bOQ")
	case tea.KeyF3:
		return []byte("\x1bOR")
	case tea.KeyF4:
		return []byte("\x1bOS")
	case tea.KeyF5:
		return []byte("\x1b[15~")
	case tea.KeyF6:
		return []byte("\x1b[17~")
	case tea.KeyF7:
		return []byte("\x1b[18~")
	case tea.KeyF8:
		return []byte("\x1b[19~")
	case tea.KeyF9:
		return []byte("\x1b[20~")
	case tea.KeyF10:
		return []byte("\x1b[21~")
	case tea.KeyF11:
		return []byte("\x1b[23~")
	case tea.KeyF12:
		return []byte("\x1b[24~")
	case tea.KeyEsc:
		return []byte("\x1b")
	case tea.KeyCtrlA:
		return []byte("\x01")
	case tea.KeyCtrlC:
		return []byte("\x03")
	case tea.KeyCtrlD:
		return []byte("\x04")
	case tea.KeyCtrlE:
		return []byte("\x05")
	case tea.KeyCtrlF:
		return []byte("\x06")
	case tea.KeyCtrlG:
		return []byte("\x07")
	case tea.KeyCtrlK:
		return []byte("\x0b")
	case tea.KeyCtrlL:
		return []byte("\x0c")
	case tea.KeyCtrlO:
		return []byte("\x0f")
	case tea.KeyCtrlP:
		return []byte("\x10")
	case tea.KeyCtrlR:
		return []byte("\x12")
	case tea.KeyCtrlS:
		return []byte("\x13")
	case tea.KeyCtrlT:
		return []byte("\x14")
	case tea.KeyCtrlU:
		return []byte("\x15")
	case tea.KeyCtrlW:
		return []byte("\x17")
	case tea.KeyCtrlX:
		return []byte("\x18")
	case tea.KeyCtrlY:
		return []byte("\x19")
	case tea.KeyCtrlZ:
		return []byte("\x1a")
	}
	return nil
}

// ---- mouse ----------------------------------------------------------------

func (m Model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.sftp != nil {
		ap := m.sftp.ap()
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			ap.cursor--
			ap.clamp()
		case tea.MouseButtonWheelDown:
			ap.cursor++
			ap.clamp()
		}
		return m, nil
	}
	switch m.state {
	case viewBookmarks:
		switch msg.Button {
		case tea.MouseButtonLeft:
			if msg.Action == tea.MouseActionRelease {
				m.bookmarks.ClickAt(msg.Y)
			}
		case tea.MouseButtonWheelUp:
			m.bookmarks.Up()
		case tea.MouseButtonWheelDown:
			m.bookmarks.Down()
		}

	case viewTerminal:
		switch msg.Button {
		case tea.MouseButtonLeft:
			pIdx := m.paneAtPos(msg.X, msg.Y)
			vtX, vtY := m.screenToVT(msg.X, msg.Y, pIdx)
			switch msg.Action {
			case tea.MouseActionPress:
				m.active = pIdx
				if pIdx < len(m.panes) {
					for i := range m.panes {
						if i != pIdx {
							m.panes[i].ClearSel()
						}
					}
					m.panes[pIdx].StartSel(vtX, vtY)
				}
			case tea.MouseActionMotion:
				if pIdx < len(m.panes) {
					m.panes[pIdx].UpdateSel(vtX, vtY)
				}
			case tea.MouseActionRelease:
				if pIdx < len(m.panes) {
					text := m.panes[pIdx].EndSel(vtX, vtY)
					if text != "" {
						_ = clipboard.WriteAll(text)
					}
				}
			}
		case tea.MouseButtonRight:
			if msg.Action == tea.MouseActionRelease && m.active < len(m.panes) {
				text, _ := clipboard.ReadAll()
				if text != "" {
					data := []byte(text)
					if m.syncInput {
						for i := range m.panes {
							m.panes[i].WriteInput(data)
						}
					} else {
						m.panes[m.active].WriteInput(data)
					}
				}
			}
		case tea.MouseButtonWheelUp:
			if m.active < len(m.panes) {
				m.panes[m.active].ScrollUp(3)
			}
		case tea.MouseButtonWheelDown:
			if m.active < len(m.panes) {
				m.panes[m.active].ScrollDown(3)
			}
		}
	}
	return m, nil
}

func (m Model) screenToVT(screenX, screenY, paneIdx int) (vtX, vtY int) {
	left, top := treeOrigin(m.root, paneIdx, 0, 0, m.width, m.height-1)
	vtX = screenX - left - 1
	vtY = screenY - top - 1
	if paneIdx < len(m.panes) && m.panes[paneIdx].showTS {
		vtX -= tsPrefixWidth
	}
	return
}

// ---- layout tree helpers --------------------------------------------------

// treeInsert splits the active-pane leaf into a split node with the new pane as the right/bottom child.
func treeInsert(node *tileNode, active int, dir splitDir, newIdx int) *tileNode {
	if node == nil {
		return &tileNode{pane: newIdx}
	}
	if node.dir == splitNone {
		if node.pane == active {
			return &tileNode{dir: dir, left: node, right: &tileNode{pane: newIdx}}
		}
		return node
	}
	return &tileNode{
		dir:   node.dir,
		left:  treeInsert(node.left, active, dir, newIdx),
		right: treeInsert(node.right, active, dir, newIdx),
	}
}

// treeRemove removes the leaf for pane removed and reindexes all higher pane indices.
func treeRemove(node *tileNode, removed int) *tileNode {
	if node == nil {
		return nil
	}
	if node.dir == splitNone {
		if node.pane == removed {
			return nil
		}
		if node.pane > removed {
			return &tileNode{pane: node.pane - 1}
		}
		return node
	}
	l := treeRemove(node.left, removed)
	r := treeRemove(node.right, removed)
	if l == nil {
		return r
	}
	if r == nil {
		return l
	}
	return &tileNode{dir: node.dir, left: l, right: r}
}

// treeResize walks the tree and calls Resize on each pane.
func (m *Model) treeResize(node *tileNode, x, y, w, h int) {
	if node == nil || w < 1 || h < 1 {
		return
	}
	if node.dir == splitNone {
		if node.pane < len(m.panes) {
			m.panes[node.pane].Resize(w, h)
		}
		return
	}
	if node.dir == splitVertical {
		lw := w / 2
		m.treeResize(node.left, x, y, lw, h)
		m.treeResize(node.right, x+lw, y, w-lw, h)
	} else {
		th := h / 2
		m.treeResize(node.left, x, y, w, th)
		m.treeResize(node.right, x, y+th, w, h-th)
	}
}

// tunnelBadge returns the badge string for a pane (e.g. " [2↗]").
func (m Model) tunnelBadge(paneUID int) string {
	count := 0
	for _, t := range m.tunnels {
		if t.paneID == paneUID && t.status == tunnelActive {
			count++
		}
	}
	if count == 0 {
		return ""
	}
	return styleTunnelBadge.Render(fmt.Sprintf(" [%d↗]", count))
}

// treeRender recursively renders the layout tree into a lipgloss string.
func (m Model) treeRender(node *tileNode) string {
	if node == nil {
		return ""
	}
	if node.dir == splitNone {
		if node.pane < len(m.panes) {
			badge := m.tunnelBadge(m.panes[node.pane].id)
			if m.syncInput {
				badge += styleSyncBadge.Render(" [sync]")
			}
			return m.panes[node.pane].View(node.pane == m.active, badge)
		}
		return ""
	}
	l, r := m.treeRender(node.left), m.treeRender(node.right)
	if node.dir == splitVertical {
		return lipgloss.JoinHorizontal(lipgloss.Top, l, r)
	}
	return lipgloss.JoinVertical(lipgloss.Left, l, r)
}

// treeAtXY returns the pane index at screen position (x,y).
func treeAtXY(node *tileNode, x, y, ox, oy, w, h int) int {
	if node == nil {
		return 0
	}
	if node.dir == splitNone {
		return node.pane
	}
	if node.dir == splitVertical {
		lw := w / 2
		if x < ox+lw {
			return treeAtXY(node.left, x, y, ox, oy, lw, h)
		}
		return treeAtXY(node.right, x, y, ox+lw, oy, w-lw, h)
	}
	th := h / 2
	if y < oy+th {
		return treeAtXY(node.left, x, y, ox, oy, w, th)
	}
	return treeAtXY(node.right, x, y, ox, oy+th, w, h-th)
}

// treeOrigin returns the top-left screen coordinate of a pane.
func treeOrigin(node *tileNode, pane, ox, oy, w, h int) (int, int) {
	if node == nil {
		return 0, 0
	}
	if node.dir == splitNone {
		return ox, oy
	}
	if node.dir == splitVertical {
		lw := w / 2
		if treeContains(node.left, pane) {
			return treeOrigin(node.left, pane, ox, oy, lw, h)
		}
		return treeOrigin(node.right, pane, ox+lw, oy, w-lw, h)
	}
	th := h / 2
	if treeContains(node.left, pane) {
		return treeOrigin(node.left, pane, ox, oy, w, th)
	}
	return treeOrigin(node.right, pane, ox, oy+th, w, h-th)
}

func treeContains(node *tileNode, pane int) bool {
	if node == nil {
		return false
	}
	if node.dir == splitNone {
		return node.pane == pane
	}
	return treeContains(node.left, pane) || treeContains(node.right, pane)
}

// paneCenter holds a pane's screen centre, used for spatial navigation.
type paneCenter struct{ pane, cx, cy int }

func treeCenters(node *tileNode, ox, oy, w, h int) []paneCenter {
	if node == nil {
		return nil
	}
	if node.dir == splitNone {
		return []paneCenter{{pane: node.pane, cx: ox + w/2, cy: oy + h/2}}
	}
	if node.dir == splitVertical {
		lw := w / 2
		return append(treeCenters(node.left, ox, oy, lw, h),
			treeCenters(node.right, ox+lw, oy, w-lw, h)...)
	}
	th := h / 2
	return append(treeCenters(node.left, ox, oy, w, th),
		treeCenters(node.right, ox, oy+th, w, h-th)...)
}

func (m Model) navigateDir(dir string) int {
	usableH := m.height - 1
	centers := treeCenters(m.root, 0, 0, m.width, usableH)
	var ax, ay int
	for _, c := range centers {
		if c.pane == m.active {
			ax, ay = c.cx, c.cy
		}
	}
	best, bestScore := m.active, -1
	for _, c := range centers {
		if c.pane == m.active {
			continue
		}
		dx, dy := c.cx-ax, c.cy-ay
		score := -1
		switch dir {
		case "left":
			if dx < 0 {
				score = -dx*100 - iabs(dy)
			}
		case "right":
			if dx > 0 {
				score = dx*100 - iabs(dy)
			}
		case "up":
			if dy < 0 {
				score = -dy*100 - iabs(dx)
			}
		case "down":
			if dy > 0 {
				score = dy*100 - iabs(dx)
			}
		}
		if score > bestScore {
			bestScore, best = score, c.pane
		}
	}
	return best
}

func iabs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// ---- pane management ------------------------------------------------------

func (m Model) openPane(bm config.Bookmark) (Model, tea.Cmd) {
	m.paneIDSeq++
	p := NewPane(m.paneIDSeq, bm)
	newIdx := len(m.panes)
	prevActive := m.active // capture before changing
	m.panes = append(m.panes, p)
	m.active = newIdx
	m.state = viewTerminal

	if m.root == nil {
		m.root = &tileNode{pane: newIdx}
	} else {
		dir := m.pendingSplit
		if dir == splitNone {
			dir = splitVertical
		}
		m.root = treeInsert(m.root, prevActive, dir, newIdx)
	}
	m.pendingSplit = splitNone
	m.resizePanes()
	return m, m.panes[m.active].Connect()
}

func (m Model) closePaneAt(idx int) (Model, tea.Cmd) {
	if idx >= len(m.panes) {
		return m, nil
	}
	paneUID := m.panes[idx].id
	m.panes[idx].Close()
	m.panes = append(m.panes[:idx], m.panes[idx+1:]...)
	m.root = treeRemove(m.root, idx)
	// Close all tunnels belonging to this pane
	remaining := make([]Tunnel, 0, len(m.tunnels))
	for _, t := range m.tunnels {
		if t.paneID == paneUID {
			if t.listener != nil {
				t.listener.Close()
			}
		} else {
			remaining = append(remaining, t)
		}
	}
	m.tunnels = remaining
	if m.tunnelModal != nil && m.tunnelModal.paneID == paneUID {
		m.tunnelModal = nil
	}
	if len(m.panes) == 0 {
		m.state = viewBookmarks
		m.root = nil
		m.active = 0
	} else {
		if m.active > idx {
			m.active-- // shift down for removed pane
		} else if m.active >= len(m.panes) {
			m.active = len(m.panes) - 1
		}
		m.resizePanes()
	}
	return m, nil
}

func (m Model) closeActivePane() (Model, tea.Cmd) {
	return m.closePaneAt(m.active)
}

func (m *Model) saveSftpDirs() {
	if m.sftp == nil {
		return
	}
	if m.sftpLastRemote == nil {
		m.sftpLastRemote = make(map[int]string)
	}
	m.sftpLastLocal = m.sftp.local.cwd
	m.sftpLastRemote[m.sftp.paneID] = m.sftp.remote.cwd
}

func (m *Model) closeAll() {
	for i := range m.panes {
		m.panes[i].Close()
	}
	for _, h := range m.sftpClients {
		h.client.Close()
	}
	for _, t := range m.tunnels {
		if t.listener != nil {
			t.listener.Close()
		}
	}
}

func (m *Model) saveTunnelToConfig(t Tunnel) {
	for bi, bm := range m.cfg.Bookmarks {
		for _, p := range m.panes {
			if p.id == t.paneID && p.bookmark.Name == bm.Name {
				for _, existing := range m.cfg.Bookmarks[bi].Tunnels {
					if existing == t.spec {
						return // already saved
					}
				}
				m.cfg.Bookmarks[bi].Tunnels = append(m.cfg.Bookmarks[bi].Tunnels, t.spec)
				config.Save("", m.cfg)
				return
			}
		}
	}
}

// popAndStartTransfer dequeues the next transfer job and starts it.
func (m *Model) popAndStartTransfer() tea.Cmd {
	if len(m.xferQueue) == 0 {
		return nil
	}
	job := m.xferQueue[0]
	m.xferQueue = m.xferQueue[1:]
	if job.dir == xferDownload {
		return startDownload(job.id, job.client, job.remotePath, job.localPath)
	}
	return startUpload(job.id, job.client, job.localPath, job.remotePath)
}

// labelFor returns a short display label for a transfer job.
func labelFor(job xferJob) string {
	if job.dir == xferDownload {
		return path.Base(job.remotePath)
	}
	return filepath.Base(job.localPath)
}

// transferStatus builds the transfer summary fragment for the status bar.
func (m Model) transferStatus() string {
	var parts []string
	for _, t := range m.transfers {
		switch t.status {
		case xferActive:
			if t.total > 0 {
				pct := t.done * 100 / t.total
				parts = append(parts, fmt.Sprintf("%s %s %d%%", t.dir.arrow(), t.label, pct))
			} else {
				parts = append(parts, fmt.Sprintf("%s %s", t.dir.arrow(), t.label))
			}
		case xferDone:
			parts = append(parts, fmt.Sprintf("✓ %s", t.label))
		case xferFailed:
			parts = append(parts, fmt.Sprintf("✗ %s", t.label))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "  " + strings.Join(parts, "  ")
}

func (m *Model) resizePanes() {
	if m.root == nil {
		return
	}
	m.treeResize(m.root, 0, 0, m.width, m.height-1)
}

func (m Model) paneAtPos(x, y int) int {
	return treeAtXY(m.root, x, y, 0, 0, m.width, m.height-1)
}

// ---- view -----------------------------------------------------------------

func (m Model) View() string {
	switch m.state {
	case viewBookmarks:
		return m.bookmarks.View()
	case viewTerminal:
		return m.terminalView()
	}
	return ""
}

func (m Model) terminalView() string {
	if len(m.panes) == 0 || m.root == nil {
		return "no panes"
	}

	body := m.treeRender(m.root)

	bodyH := m.height - 1
	// Overlay tunnel modal when open
	if m.tunnelModal != nil {
		modal := m.tunnelModal.view(m.width, bodyH, m.tunnels)
		body = lipgloss.Place(m.width, bodyH, lipgloss.Center, lipgloss.Center, modal)
	} else if m.sftp != nil {
		modal := m.sftp.view(m.width, bodyH)
		body = lipgloss.Place(m.width, bodyH, lipgloss.Center, lipgloss.Center, modal)
	}

	prefix := ""
	if m.prefix == prefixCtrlB {
		prefix = " [^B]"
	}
	xfers := m.transferStatus()
	status := fmt.Sprintf(
		"%s [%d/%d]%s  ^B %%/\" split  ^B hjkl nav  ^B f sftp  ^B T tunnels  ^B x close  ^B t ts  ^B b bookmarks  ^Q quit",
		prefix, m.active+1, len(m.panes), xfers,
	)
	bar := styleStatusBar.Width(m.width).Render(status)

	return lipgloss.JoinVertical(lipgloss.Left, body, bar)
}

func Run(cfg *config.Config) error {
	m := NewModel(cfg)
	p := tea.NewProgram(
		m,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)
	_, err := p.Run()
	return err
}

