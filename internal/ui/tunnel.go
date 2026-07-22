package ui

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"

	"dterm/internal/config"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	gossh "golang.org/x/crypto/ssh"
)

// ---- types ------------------------------------------------------------------

type tunnelStatus int

const (
	tunnelStarting tunnelStatus = iota
	tunnelActive
	tunnelError
	tunnelStopped
)

type Tunnel struct {
	id       int
	paneID   int
	spec     config.TunnelSpec
	status   tunnelStatus
	errMsg   string
	saved    bool
	listener net.Listener // non-nil when active; Close() stops the accept loop
}

// ---- messages ---------------------------------------------------------------

type tunnelReadyMsg struct {
	id       int
	listener net.Listener
	next     tea.Cmd
}

type tunnelAcceptMsg struct {
	id   int
	next tea.Cmd
}

type tunnelErrorMsg struct {
	id  int
	err error
}

type tunnelStartRequestMsg struct {
	paneID int
	spec   config.TunnelSpec
}

type tunnelCloseRequestMsg struct{ id int }

type tunnelDeleteRequestMsg struct{ id int }

type tunnelSaveRequestMsg struct{ id int }

type tunnelRetryRequestMsg struct{ id int }

type tunnelEditRequestMsg struct {
	id   int
	spec config.TunnelSpec
}

// ---- accept loops -----------------------------------------------------------

func localAcceptCmd(id int, l net.Listener, client *gossh.Client, spec config.TunnelSpec) tea.Cmd {
	return func() tea.Msg {
		conn, err := l.Accept()
		if err != nil {
			return tunnelErrorMsg{id: id, err: err}
		}
		if spec.Type == "dynamic" {
			go handleSOCKS5(conn, client)
		} else {
			target := fmt.Sprintf("%s:%d", spec.RemoteHost, spec.RemotePort)
			go func() {
				defer func() { recover() }()
				remote, err := client.Dial("tcp", target)
				if err != nil {
					conn.Close()
					return
				}
				proxyConns(conn, remote)
			}()
		}
		next := localAcceptCmd(id, l, client, spec)
		return tunnelAcceptMsg{id: id, next: next}
	}
}

func remoteAcceptCmd(id int, l net.Listener, spec config.TunnelSpec) tea.Cmd {
	return func() tea.Msg {
		conn, err := l.Accept()
		if err != nil {
			return tunnelErrorMsg{id: id, err: err}
		}
		localHost := spec.LocalHost
		if localHost == "" {
			localHost = "127.0.0.1"
		}
		target := net.JoinHostPort(localHost, strconv.Itoa(spec.LocalPort))
		go func() {
			defer func() { recover() }()
			local, err := net.Dial("tcp", target)
			if err != nil {
				conn.Close()
				return
			}
			proxyConns(conn, local)
		}()
		next := remoteAcceptCmd(id, l, spec)
		return tunnelAcceptMsg{id: id, next: next}
	}
}

// startTunnelCmd binds the listener and returns tunnelReadyMsg on success.
func startTunnelCmd(id int, spec config.TunnelSpec, client *gossh.Client) tea.Cmd {
	return func() tea.Msg {
		switch spec.Type {
		case "local", "dynamic":
			addr := fmt.Sprintf("127.0.0.1:%d", spec.LocalPort)
			l, err := net.Listen("tcp", addr)
			if err != nil {
				return tunnelErrorMsg{id: id, err: fmt.Errorf("listen %s: %w", addr, err)}
			}
			next := localAcceptCmd(id, l, client, spec)
			return tunnelReadyMsg{id: id, listener: l, next: next}
		case "remote":
			remoteHost := "0.0.0.0"
			addr := fmt.Sprintf("%s:%d", remoteHost, spec.RemotePort)
			l, err := client.Listen("tcp", addr)
			if err != nil {
				return tunnelErrorMsg{id: id, err: fmt.Errorf("remote listen %s: %w", addr, err)}
			}
			next := remoteAcceptCmd(id, l, spec)
			return tunnelReadyMsg{id: id, listener: l, next: next}
		}
		return tunnelErrorMsg{id: id, err: fmt.Errorf("unknown tunnel type %q", spec.Type)}
	}
}

// ---- SOCKS5 proxy -----------------------------------------------------------

func handleSOCKS5(conn net.Conn, client *gossh.Client) {
	defer conn.Close()
	buf := make([]byte, 256)

	// Version + nMethods
	if _, err := io.ReadFull(conn, buf[:2]); err != nil || buf[0] != 5 {
		return
	}
	nMethods := int(buf[1])
	if _, err := io.ReadFull(conn, buf[:nMethods]); err != nil {
		return
	}
	// No auth required
	conn.Write([]byte{5, 0})

	// Request header: ver cmd rsv atyp
	if _, err := io.ReadFull(conn, buf[:4]); err != nil {
		return
	}
	if buf[0] != 5 || buf[1] != 1 { // must be CONNECT
		return
	}

	var host string
	switch buf[3] {
	case 1: // IPv4
		if _, err := io.ReadFull(conn, buf[:4]); err != nil {
			return
		}
		host = net.IP(buf[:4]).String()
	case 3: // domain
		if _, err := io.ReadFull(conn, buf[:1]); err != nil {
			return
		}
		n := int(buf[0])
		if _, err := io.ReadFull(conn, buf[:n]); err != nil {
			return
		}
		host = string(buf[:n])
	case 4: // IPv6
		if _, err := io.ReadFull(conn, buf[:16]); err != nil {
			return
		}
		host = "[" + net.IP(buf[:16]).String() + "]"
	default:
		return
	}

	var portBuf [2]byte
	if _, err := io.ReadFull(conn, portBuf[:]); err != nil {
		return
	}
	port := binary.BigEndian.Uint16(portBuf[:])
	target := fmt.Sprintf("%s:%d", host, port)

	remote, err := client.Dial("tcp", target)
	if err != nil {
		// reply: host unreachable
		conn.Write([]byte{5, 4, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer remote.Close()
	// reply: success, bound addr 0.0.0.0:0
	conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	proxyConns(conn, remote)
}

func proxyConns(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { io.Copy(a, b); done <- struct{}{} }()
	go func() { io.Copy(b, a); done <- struct{}{} }()
	<-done
	a.Close()
	b.Close()
}

// ---- spec helpers -----------------------------------------------------------

// specLabel returns a compact one-line description of the tunnel spec.
func specLabel(s config.TunnelSpec) string {
	switch s.Type {
	case "local":
		return fmt.Sprintf("L:%d→%s:%d", s.LocalPort, s.RemoteHost, s.RemotePort)
	case "remote":
		lh := s.LocalHost
		if lh == "" {
			lh = "localhost"
		}
		return fmt.Sprintf("R:%d←%s:%d", s.RemotePort, lh, s.LocalPort)
	case "dynamic":
		return fmt.Sprintf("D:%d (SOCKS5)", s.LocalPort)
	}
	return s.Type
}

// specToInput converts a TunnelSpec back to the user-facing shorthand string.
func specToInput(spec config.TunnelSpec) string {
	var s string
	switch spec.Type {
	case "local":
		s = fmt.Sprintf("L:%d:%s:%d", spec.LocalPort, spec.RemoteHost, spec.RemotePort)
	case "remote":
		lh := spec.LocalHost
		if lh == "" {
			lh = "localhost"
		}
		s = fmt.Sprintf("R:%d:%s:%d", spec.RemotePort, lh, spec.LocalPort)
	case "dynamic":
		s = fmt.Sprintf("D:%d", spec.LocalPort)
	default:
		s = spec.Type
	}
	if spec.Name != "" {
		return spec.Name + "@" + s
	}
	return s
}

// parseTunnelSpec parses the shorthand notation typed by the user:
//
//	[name@]L:localPort:remoteHost:remotePort
//	[name@]R:remotePort:localHost:localPort
//	[name@]D:localPort
func parseTunnelSpec(raw string) (config.TunnelSpec, error) {
	raw = strings.TrimSpace(raw)

	// Extract optional name prefix: "myname@L:..."
	var name string
	if at := strings.Index(raw, "@"); at > 0 {
		name = strings.TrimSpace(raw[:at])
		raw = strings.TrimSpace(raw[at+1:])
	}

	parts := strings.SplitN(raw, ":", 5)
	if len(parts) < 2 {
		return config.TunnelSpec{}, fmt.Errorf("use L:port:host:port / R:port:host:port / D:port")
	}
	typ := strings.ToLower(parts[0])
	atoi := func(s string) (int, error) {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n < 1 || n > 65535 {
			return 0, fmt.Errorf("invalid port %q", s)
		}
		return n, nil
	}
	switch typ {
	case "l", "local":
		if len(parts) < 4 {
			return config.TunnelSpec{}, fmt.Errorf("local: L:localPort:remoteHost:remotePort")
		}
		lp, err := atoi(parts[1])
		if err != nil {
			return config.TunnelSpec{}, err
		}
		rp, err := atoi(parts[3])
		if err != nil {
			return config.TunnelSpec{}, err
		}
		return config.TunnelSpec{Name: name, Type: "local", LocalPort: lp, RemoteHost: strings.TrimSpace(parts[2]), RemotePort: rp}, nil
	case "r", "remote":
		if len(parts) < 4 {
			return config.TunnelSpec{}, fmt.Errorf("remote: R:remotePort:localHost:localPort")
		}
		rp, err := atoi(parts[1])
		if err != nil {
			return config.TunnelSpec{}, err
		}
		lp, err := atoi(parts[3])
		if err != nil {
			return config.TunnelSpec{}, err
		}
		return config.TunnelSpec{Name: name, Type: "remote", RemotePort: rp, LocalHost: strings.TrimSpace(parts[2]), LocalPort: lp}, nil
	case "d", "dynamic":
		lp, err := atoi(parts[1])
		if err != nil {
			return config.TunnelSpec{}, err
		}
		return config.TunnelSpec{Name: name, Type: "dynamic", LocalPort: lp}, nil
	}
	return config.TunnelSpec{}, fmt.Errorf("unknown type %q — use L, R, or D", parts[0])
}

// ---- modal ------------------------------------------------------------------

const (
	inputModeNone = 0
	inputModeAdd  = 1
	inputModeEdit = 2
)

type tunnelModal struct {
	paneID    int
	paneLabel string // "user@host" for display
	cursor    int
	addInput  textinput.Model
	addErr    string
	inputMode int // inputModeNone / inputModeAdd / inputModeEdit
	editID    int // tunnel id being edited (inputModeEdit only)
}

func newTunnelModal(paneID int, paneLabel string) *tunnelModal {
	ti := textinput.New()
	ti.Placeholder = "L:5432:db.host:5432  or  R:8080:localhost:3000  or  D:1080"
	ti.CharLimit = 128
	ti.Width = 52
	return &tunnelModal{paneID: paneID, paneLabel: paneLabel, addInput: ti}
}

func (m *tunnelModal) paneTunnels(all []Tunnel) []Tunnel {
	var out []Tunnel
	for _, t := range all {
		if t.paneID == m.paneID {
			out = append(out, t)
		}
	}
	return out
}

// update handles a key message. Returns (cmd, close).
func (m *tunnelModal) update(msg tea.KeyMsg, all []Tunnel) (tea.Cmd, bool) {
	tunnels := m.paneTunnels(all)

	if m.inputMode != inputModeNone {
		return m.updateInputMode(msg)
	}

	switch msg.String() {
	case "esc", "q":
		return nil, true

	case "j", "down":
		m.cursor++
		if m.cursor >= len(tunnels) {
			m.cursor = len(tunnels) - 1
		}
		if m.cursor < 0 {
			m.cursor = 0
		}

	case "k", "up":
		m.cursor--
		if m.cursor < 0 {
			m.cursor = 0
		}

	case "a", "i":
		m.inputMode = inputModeAdd
		m.addInput.SetValue("")
		m.addErr = ""
		m.addInput.Focus()

	case "enter", " ":
		if len(tunnels) == 0 {
			m.inputMode = inputModeAdd
			m.addInput.SetValue("")
			m.addErr = ""
			m.addInput.Focus()
			break
		}
		if m.cursor < len(tunnels) {
			t := tunnels[m.cursor]
			switch t.status {
			case tunnelActive, tunnelStarting:
				return func() tea.Msg { return tunnelCloseRequestMsg{id: t.id} }, false
			case tunnelStopped, tunnelError:
				return func() tea.Msg { return tunnelRetryRequestMsg{id: t.id} }, false
			}
		}

	case "e":
		if m.cursor < len(tunnels) {
			t := tunnels[m.cursor]
			m.inputMode = inputModeEdit
			m.editID = t.id
			m.addInput.SetValue(specToInput(t.spec))
			m.addErr = ""
			m.addInput.Focus()
		}

	case "x":
		if m.cursor < len(tunnels) {
			id := tunnels[m.cursor].id
			return func() tea.Msg { return tunnelDeleteRequestMsg{id: id} }, false
		}

	case "s":
		if m.cursor < len(tunnels) && !tunnels[m.cursor].saved {
			id := tunnels[m.cursor].id
			return func() tea.Msg { return tunnelSaveRequestMsg{id: id} }, false
		}
	}
	return nil, false
}

func (m *tunnelModal) updateInputMode(msg tea.KeyMsg) (tea.Cmd, bool) {
	switch msg.String() {
	case "esc":
		m.inputMode = inputModeNone
		m.addInput.Blur()
		m.addErr = ""
		return nil, false

	case "enter":
		spec, err := parseTunnelSpec(m.addInput.Value())
		if err != nil {
			m.addErr = err.Error()
			return nil, false
		}
		mode := m.inputMode
		m.inputMode = inputModeNone
		m.addInput.Blur()
		m.addErr = ""
		if mode == inputModeEdit {
			id := m.editID
			return func() tea.Msg { return tunnelEditRequestMsg{id: id, spec: spec} }, false
		}
		paneID := m.paneID
		return func() tea.Msg { return tunnelStartRequestMsg{paneID: paneID, spec: spec} }, false

	default:
		var cmd tea.Cmd
		m.addInput, cmd = m.addInput.Update(msg)
		m.addErr = ""
		return cmd, false
	}
}

func (m *tunnelModal) view(totalW, totalH int, all []Tunnel) string {
	tunnels := m.paneTunnels(all)

	mw := totalW * 3 / 5
	if mw < 64 {
		mw = 64
	}
	if mw > totalW {
		mw = totalW
	}
	mh := len(tunnels) + 8 // list + add row + hints + borders
	if mh < 12 {
		mh = 12
	}
	if mh > totalH {
		mh = totalH
	}

	innerW := mw - 2
	innerH := mh - 2

	// Title
	titleText := " Tunnels: " + m.paneLabel + " "
	title := styleSftpPathActive.Render(titleText)

	// Tunnel list
	var listLines []string
	for i, t := range tunnels {
		label := specLabel(t.spec)
		if t.spec.Name != "" {
			label = t.spec.Name + "  " + label
		}

		var statusStr string
		var statusStyle lipgloss.Style
		switch t.status {
		case tunnelStarting:
			statusStr = "starting…"
			statusStyle = styleHelp
		case tunnelActive:
			statusStr = "● active"
			statusStyle = styleTunnelActive
		case tunnelError:
			statusStr = "✕ " + t.errMsg
			if len([]rune(statusStr)) > 24 {
				statusStr = string([]rune(statusStr)[:23]) + "…"
			}
			statusStyle = styleTunnelError
		case tunnelStopped:
			statusStr = "○ stopped"
			statusStyle = styleHelp
		}

		var actions string
		switch {
		case t.saved:
			actions = " ✓"
		case !t.saved:
			actions = " [s]"
		}

		maxLabel := innerW - len([]rune(statusStr)) - len([]rune(actions)) - 4
		if maxLabel < 1 {
			maxLabel = 1
		}
		labelRunes := []rune(label)
		if len(labelRunes) > maxLabel {
			label = string(labelRunes[:maxLabel-1]) + "…"
		}
		line := fmt.Sprintf("  %-*s  %s%s", maxLabel, label, statusStr, actions)
		_ = statusStyle

		var rendered string
		if i == m.cursor && m.inputMode == inputModeNone {
			rendered = styleSftpSelActive.Width(innerW).Render(line)
		} else {
			base := styleTunnelRow
			if t.status == tunnelError {
				base = styleTunnelRowErr
			} else if t.status == tunnelStopped {
				base = styleHelp
			}
			rendered = base.Width(innerW).Render(line)
		}
		listLines = append(listLines, rendered)
	}

	if len(listLines) == 0 {
		listLines = []string{styleHelp.Width(innerW).Render("  no tunnels — press 'a' to add one")}
	}

	// Add/Edit input row
	var addRow string
	if m.inputMode != inputModeNone {
		m.addInput.Width = innerW - 9
		prefix := "  Add:  "
		if m.inputMode == inputModeEdit {
			prefix = "  Edit: "
		}
		addRow = styleSftpPathActive.Width(innerW).Render(prefix + m.addInput.View())
	} else {
		addRow = styleHelp.Width(innerW).Render("  [a] add  [enter] toggle  [e] edit  [x] delete")
	}
	if m.addErr != "" {
		addRow += "\n" + styleTunnelError.Width(innerW).Render("  ✕ "+m.addErr)
	}

	// Hints
	var hintText string
	if m.inputMode != inputModeNone {
		hintText = "enter confirm  esc cancel"
	} else {
		hintText = "j/k move  a add  enter toggle  e edit  x del  s save  esc exit"
	}
	hints := styleHelp.Width(innerW).Render(hintText)
	hdiv := styleSftpSep.Render(strings.Repeat("─", innerW))

	parts := append([]string{title}, listLines...)
	parts = append(parts, hdiv, addRow, hdiv, hints)

	// Pad to innerH
	content := strings.Join(parts, "\n")
	return styleSftpModal.Width(innerW).Height(innerH).Render(content)
}
