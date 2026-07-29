package ui

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/pkg/sftp"
	gossh "golang.org/x/crypto/ssh"
)

// ---- transfer types ---------------------------------------------------------

type xferDir int

const (
	xferDownload xferDir = iota // remote → local
	xferUpload                  // local → remote
)

func (d xferDir) arrow() string {
	if d == xferDownload {
		return "↓"
	}
	return "↑"
}

type xferStatus int

const (
	xferActive xferStatus = iota
	xferDone
	xferFailed
)

type Transfer struct {
	id     int
	label  string
	dir    xferDir
	done   int64
	total  int64
	status xferStatus
	err    error
}

type xferJob struct {
	id         int
	localPath  string
	remotePath string
	size       int64
	dir        xferDir
	client     *sftp.Client
}

// ---- messages ---------------------------------------------------------------

type sftpOpenedMsg struct {
	paneID int
	client *sftp.Client
	err    error
}

type sftpDirLoadedMsg struct {
	panel   int // 0=local, 1=remote
	cwd     string
	entries []sftpEntry
	err     error
}

type sftpScanDoneMsg struct {
	label string
	jobs  []xferJob
	total int64
}

type sftpScanErrMsg struct{ err error }

type sftpProgressMsg struct {
	id    int
	done  int64
	total int64
	next  tea.Cmd
}

type sftpXferDoneMsg struct {
	id  int
	err error
}

type sftpClearMsg struct{ id int }

type sftpConfirmedMsg struct{ jobs []xferJob }

type sftpCompletionsMsg struct {
	panel   int
	entries []string
}

type sftpDeletedMsg struct {
	panel int
	cwd   string
	err   error
}

type sftpMkdirDoneMsg struct {
	panel int
	cwd   string
	err   error
}

// ---- entry & panel ----------------------------------------------------------

type sftpEntry struct {
	name    string
	size    int64
	isDir   bool
	modTime time.Time
}

type sftpPanel struct {
	cwd         string
	entries     []sftpEntry
	cursor      int
	offset      int
	pathInput   textinput.Model
	editing     bool
	completions []string
	compIdx     int
}

func newSftpPanel(cwd string) sftpPanel {
	ti := textinput.New()
	ti.Prompt = " "
	ti.CharLimit = 512
	return sftpPanel{cwd: cwd, pathInput: ti}
}

func (p *sftpPanel) selected() *sftpEntry {
	if p.cursor >= 0 && p.cursor < len(p.entries) {
		return &p.entries[p.cursor]
	}
	return nil
}

func (p *sftpPanel) clamp() {
	n := len(p.entries)
	if n == 0 {
		p.cursor = 0
		return
	}
	if p.cursor < 0 {
		p.cursor = 0
	}
	if p.cursor >= n {
		p.cursor = n - 1
	}
}

func (p *sftpPanel) scrollToView(rows int) {
	if rows < 1 {
		return
	}
	if p.cursor < p.offset {
		p.offset = p.cursor
	}
	if p.cursor >= p.offset+rows {
		p.offset = p.cursor - rows + 1
	}
}

// ---- modal ------------------------------------------------------------------

type confirmPhase int

const (
	confirmNone     confirmPhase = iota
	confirmScanning              // waiting for scan Cmd to return
	confirmReady                 // scan done, awaiting y/n
	confirmScanErr               // scan failed
	confirmDelete                // awaiting y/n to delete the selected entry
)

type sftpModal struct {
	paneID      int
	client      *sftp.Client
	local       sftpPanel
	remote      sftpPanel
	activePanel int // 0=local, 1=remote

	confirm      confirmPhase
	confirmJobs  []xferJob
	confirmTotal int64
	confirmLabel string
	confirmMsg   string

	showTime bool // toggle modified-time column

	// Delete confirmation
	deleteTarget string
	deletePanel  int
	deleteName   string

	// Mkdir
	mkdirMode  bool
	mkdirInput textinput.Model
	mkdirErr   string
}

var sftpXferSeq int

func nextXferID() int {
	sftpXferSeq++
	return sftpXferSeq
}

func newSftpModal(paneID int, client *sftp.Client, initLocal, initRemote string) (*sftpModal, tea.Cmd) {
	if initLocal == "" {
		initLocal, _ = os.UserHomeDir()
	}
	if initRemote == "" {
		initRemote = "/"
	}
	mi := textinput.New()
	mi.Prompt = " "
	mi.Placeholder = "folder name"
	mi.CharLimit = 256
	m := &sftpModal{
		paneID:      paneID,
		client:      client,
		local:       newSftpPanel(initLocal),
		remote:      newSftpPanel(initRemote),
		activePanel: 1,
		mkdirInput:  mi,
	}
	return m, tea.Batch(
		loadLocalDir(0, initLocal),
		loadRemoteDir(client, 1, initRemote),
	)
}

func (m *sftpModal) ap() *sftpPanel {
	if m.activePanel == 0 {
		return &m.local
	}
	return &m.remote
}

func (m *sftpModal) opp() *sftpPanel {
	if m.activePanel == 0 {
		return &m.remote
	}
	return &m.local
}

// update handles a message for the modal.
// Returns (cmd, close): close=true means the caller should dismiss the modal.
func (m *sftpModal) update(msg tea.Msg) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case sftpDirLoadedMsg:
		var p *sftpPanel
		if msg.panel == 0 {
			p = &m.local
		} else {
			p = &m.remote
		}
		if msg.err == nil {
			p.cwd = msg.cwd
			p.entries = msg.entries
			p.cursor = 0
			p.offset = 0
			p.completions = nil
		}
		return nil, false

	case sftpCompletionsMsg:
		if msg.panel == m.activePanel {
			ap := m.ap()
			ap.completions = msg.entries
			ap.compIdx = 0
		}
		return nil, false

	case sftpScanDoneMsg:
		if m.confirm == confirmScanning {
			m.confirm = confirmReady
			m.confirmJobs = msg.jobs
			m.confirmTotal = msg.total
			m.confirmMsg = fmt.Sprintf("(%d files, %s)", len(msg.jobs), formatBytes(msg.total))
		}
		return nil, false

	case sftpScanErrMsg:
		if m.confirm == confirmScanning {
			m.confirm = confirmScanErr
			m.confirmMsg = msg.err.Error()
		}
		return nil, false

	case sftpDeletedMsg:
		if msg.err != nil {
			m.confirm = confirmScanErr
			m.confirmMsg = "delete failed: " + msg.err.Error()
			return nil, false
		}
		if msg.panel == 0 {
			return loadLocalDir(0, msg.cwd), false
		}
		return loadRemoteDir(m.client, 1, msg.cwd), false

	case sftpMkdirDoneMsg:
		m.mkdirMode = false
		m.mkdirInput.Blur()
		if msg.err != nil {
			m.mkdirErr = msg.err.Error()
			return nil, false
		}
		m.mkdirErr = ""
		if msg.panel == 0 {
			return loadLocalDir(0, msg.cwd), false
		}
		return loadRemoteDir(m.client, 1, msg.cwd), false

	case tea.KeyMsg:
		if m.mkdirMode {
			return m.updateMkdir(msg)
		}
		if m.confirm != confirmNone {
			return m.updateConfirm(msg)
		}
		if m.ap().editing {
			return m.updatePathBar(msg)
		}
		return m.updateBrowse(msg)
	}

	return nil, false
}

func (m *sftpModal) updateBrowse(msg tea.KeyMsg) (tea.Cmd, bool) {
	ap := m.ap()
	switch msg.String() {
	case "esc":
		return nil, true

	case "h": // focus local panel
		m.activePanel = 0
		m.remote.completions = nil

	case "l": // focus remote panel
		m.activePanel = 1
		m.local.completions = nil

	case "j":
		ap.cursor++
		ap.clamp()

	case "k":
		ap.cursor--
		ap.clamp()

	case "enter":
		if e := ap.selected(); e != nil && e.isDir {
			return m.enterDir()
		}

	case "backspace":
		return m.parentDir()

	case "ctrl+l":
		ap.pathInput.SetValue(ap.cwd)
		ap.pathInput.Width = 40
		ap.pathInput.Focus()
		ap.pathInput.CursorEnd()
		ap.editing = true

	case "r":
		return m.reload()

	case "s":
		return m.startSync()

	case "d":
		return m.startDelete()

	case "m":
		return m.startMkdir()

	case "T":
		m.showTime = !m.showTime
	}

	return nil, false
}

func (m *sftpModal) updatePathBar(msg tea.KeyMsg) (tea.Cmd, bool) {
	ap := m.ap()
	switch msg.String() {
	case "esc":
		ap.pathInput.Blur()
		ap.editing = false
		ap.completions = nil

	case "enter":
		newPath := strings.TrimSpace(ap.pathInput.Value())
		ap.pathInput.Blur()
		ap.editing = false
		ap.completions = nil
		if newPath != "" {
			return m.navigateTo(newPath)
		}

	case "tab":
		if len(ap.completions) > 0 {
			ap.compIdx = (ap.compIdx + 1) % len(ap.completions)
			ap.pathInput.SetValue(ap.completions[ap.compIdx])
			ap.pathInput.CursorEnd()
		}

	default:
		var cmd tea.Cmd
		ap.pathInput, cmd = ap.pathInput.Update(msg)
		typed := ap.pathInput.Value()
		return tea.Batch(cmd, m.fetchCompletions(typed)), false
	}

	return nil, false
}

func (m *sftpModal) updateConfirm(msg tea.KeyMsg) (tea.Cmd, bool) {
	switch msg.String() {
	case "y", "Y", "enter":
		switch m.confirm {
		case confirmReady:
			jobs := m.confirmJobs
			m.confirm = confirmNone
			m.confirmJobs = nil
			m.confirmLabel = ""
			m.confirmMsg = ""
			return func() tea.Msg { return sftpConfirmedMsg{jobs: jobs} }, false
		case confirmDelete:
			panel := m.deletePanel
			cwd := m.ap().cwd
			target := m.deleteTarget
			m.confirm = confirmNone
			m.deleteTarget = ""
			m.deleteName = ""
			if panel == 0 {
				return deleteLocalCmd(panel, cwd, target), false
			}
			return deleteRemoteCmd(m.client, panel, cwd, target), false
		}
	case "n", "N", "esc":
		m.confirm = confirmNone
		m.confirmJobs = nil
		m.confirmLabel = ""
		m.confirmMsg = ""
		m.deleteTarget = ""
		m.deleteName = ""
	}
	return nil, false
}

func (m *sftpModal) updateMkdir(msg tea.KeyMsg) (tea.Cmd, bool) {
	switch msg.String() {
	case "esc":
		m.mkdirMode = false
		m.mkdirInput.Blur()
		m.mkdirErr = ""
	case "enter":
		name := strings.TrimSpace(m.mkdirInput.Value())
		if name == "" {
			m.mkdirMode = false
			m.mkdirInput.Blur()
			m.mkdirErr = ""
			return nil, false
		}
		panel := m.activePanel
		cwd := m.ap().cwd
		if panel == 0 {
			return mkdirLocalCmd(panel, cwd, name), false
		}
		return mkdirRemoteCmd(m.client, panel, cwd, name), false
	default:
		var cmd tea.Cmd
		m.mkdirInput, cmd = m.mkdirInput.Update(msg)
		m.mkdirErr = ""
		return cmd, false
	}
	return nil, false
}

// ---- navigation helpers -----------------------------------------------------

func (m *sftpModal) enterDir() (tea.Cmd, bool) {
	ap := m.ap()
	e := ap.selected()
	if e == nil || !e.isDir {
		return nil, false
	}
	if m.activePanel == 0 {
		return loadLocalDir(0, filepath.Join(ap.cwd, e.name)), false
	}
	return loadRemoteDir(m.client, 1, path.Join(ap.cwd, e.name)), false
}

func (m *sftpModal) parentDir() (tea.Cmd, bool) {
	ap := m.ap()
	if m.activePanel == 0 {
		parent := filepath.Dir(ap.cwd)
		if parent == ap.cwd {
			return nil, false
		}
		return loadLocalDir(0, parent), false
	}
	parent := path.Dir(ap.cwd)
	if parent == ap.cwd {
		return nil, false
	}
	return loadRemoteDir(m.client, 1, parent), false
}

func (m *sftpModal) navigateTo(dest string) (tea.Cmd, bool) {
	if m.activePanel == 0 {
		return loadLocalDir(0, dest), false
	}
	return loadRemoteDir(m.client, 1, dest), false
}

func (m *sftpModal) reload() (tea.Cmd, bool) {
	ap := m.ap()
	if m.activePanel == 0 {
		return loadLocalDir(0, ap.cwd), false
	}
	return loadRemoteDir(m.client, 1, ap.cwd), false
}

func (m *sftpModal) startSync() (tea.Cmd, bool) {
	ap := m.ap()
	e := ap.selected()
	if e == nil {
		return nil, false
	}

	var localPath, remotePath string
	var dir xferDir

	if m.activePanel == 1 { // remote selected → download
		remotePath = path.Join(m.remote.cwd, e.name)
		localPath = filepath.Join(m.local.cwd, e.name)
		dir = xferDownload
	} else { // local selected → upload
		localPath = filepath.Join(m.local.cwd, e.name)
		remotePath = path.Join(m.remote.cwd, e.name)
		dir = xferUpload
	}

	if e.isDir {
		m.confirm = confirmScanning
		if dir == xferDownload {
			m.confirmLabel = fmt.Sprintf("↓ %s/ → %s", e.name, m.local.cwd)
			return scanRemoteDir(m.client, remotePath, filepath.Join(m.local.cwd, e.name), dir), false
		}
		m.confirmLabel = fmt.Sprintf("↑ %s/ → %s", e.name, m.remote.cwd)
		return scanLocalDir(localPath, path.Join(m.remote.cwd, e.name), m.client, dir), false
	}

	// Single file: queue immediately without confirmation
	job := xferJob{
		id:         nextXferID(),
		localPath:  localPath,
		remotePath: remotePath,
		size:       e.size,
		dir:        dir,
		client:     m.client,
	}
	return func() tea.Msg { return sftpConfirmedMsg{jobs: []xferJob{job}} }, false
}

func (m *sftpModal) startDelete() (tea.Cmd, bool) {
	ap := m.ap()
	e := ap.selected()
	if e == nil {
		return nil, false
	}
	var target string
	if m.activePanel == 0 {
		target = filepath.Join(ap.cwd, e.name)
	} else {
		target = path.Join(ap.cwd, e.name)
	}
	m.confirm = confirmDelete
	m.deletePanel = m.activePanel
	m.deleteTarget = target
	m.deleteName = e.name
	if e.isDir {
		m.deleteName += "/"
	}
	return nil, false
}

func (m *sftpModal) startMkdir() (tea.Cmd, bool) {
	m.mkdirMode = true
	m.mkdirErr = ""
	m.mkdirInput.SetValue("")
	m.mkdirInput.Focus()
	return nil, false
}

func (m *sftpModal) fetchCompletions(typed string) tea.Cmd {
	panel := m.activePanel
	if panel == 0 {
		return func() tea.Msg {
			dir := filepath.Dir(typed)
			prefix := filepath.Base(typed)
			entries, err := os.ReadDir(dir)
			if err != nil {
				return sftpCompletionsMsg{panel: panel}
			}
			var out []string
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), prefix) {
					p := filepath.Join(dir, e.Name())
					if e.IsDir() {
						p += string(filepath.Separator)
					}
					out = append(out, p)
				}
			}
			return sftpCompletionsMsg{panel: panel, entries: out}
		}
	}
	client := m.client
	return func() tea.Msg {
		dir := path.Dir(typed)
		prefix := path.Base(typed)
		entries, err := client.ReadDir(dir)
		if err != nil {
			return sftpCompletionsMsg{panel: panel}
		}
		var out []string
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), prefix) {
				p := path.Join(dir, e.Name())
				if e.IsDir() {
					p += "/"
				}
				out = append(out, p)
			}
		}
		return sftpCompletionsMsg{panel: panel, entries: out}
	}
}

// ---- directory loading Cmds -------------------------------------------------

func loadLocalDir(panel int, dir string) tea.Cmd {
	return func() tea.Msg {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return sftpDirLoadedMsg{panel: panel, err: err}
		}
		items := make([]sftpEntry, 0, len(entries))
		for _, e := range entries {
			info, _ := e.Info()
			var sz int64
			var mt time.Time
			if info != nil {
				if !e.IsDir() {
					sz = info.Size()
				}
				mt = info.ModTime()
			}
			items = append(items, sftpEntry{name: e.Name(), isDir: e.IsDir(), size: sz, modTime: mt})
		}
		sortEntries(items)
		return sftpDirLoadedMsg{panel: panel, cwd: dir, entries: items}
	}
}

func loadRemoteDir(client *sftp.Client, panel int, dir string) tea.Cmd {
	return func() tea.Msg {
		entries, err := client.ReadDir(dir)
		if err != nil {
			return sftpDirLoadedMsg{panel: panel, err: err}
		}
		items := make([]sftpEntry, 0, len(entries))
		for _, e := range entries {
			items = append(items, sftpEntry{
				name:    e.Name(),
				isDir:   e.IsDir(),
				size:    e.Size(),
				modTime: e.ModTime(),
			})
		}
		sortEntries(items)
		return sftpDirLoadedMsg{panel: panel, cwd: dir, entries: items}
	}
}

func sortEntries(items []sftpEntry) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].isDir != items[j].isDir {
			return items[i].isDir
		}
		return items[i].name < items[j].name
	})
}

// ---- scan Cmds --------------------------------------------------------------

func scanRemoteDir(client *sftp.Client, remotePath, localBase string, dir xferDir) tea.Cmd {
	return func() tea.Msg {
		label := path.Base(remotePath) + "/"
		var jobs []xferJob
		var total int64
		walker := client.Walk(remotePath)
		for walker.Step() {
			if walker.Err() != nil {
				continue
			}
			if walker.Stat().IsDir() {
				continue
			}
			rel, err := filepath.Rel(remotePath, walker.Path())
			if err != nil {
				rel = walker.Stat().Name()
			}
			jobs = append(jobs, xferJob{
				id:         nextXferID(),
				remotePath: walker.Path(),
				localPath:  filepath.Join(localBase, filepath.FromSlash(rel)),
				size:       walker.Stat().Size(),
				dir:        dir,
				client:     client,
			})
			total += walker.Stat().Size()
		}
		return sftpScanDoneMsg{label: label, jobs: jobs, total: total}
	}
}

func scanLocalDir(localPath, remoteDest string, client *sftp.Client, dir xferDir) tea.Cmd {
	return func() tea.Msg {
		label := filepath.Base(localPath) + "/"
		var jobs []xferJob
		var total int64
		err := filepath.Walk(localPath, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(localPath, p)
			jobs = append(jobs, xferJob{
				id:         nextXferID(),
				localPath:  p,
				remotePath: remoteDest + "/" + filepath.ToSlash(rel),
				size:       info.Size(),
				dir:        dir,
				client:     client,
			})
			total += info.Size()
			return nil
		})
		if err != nil {
			return sftpScanErrMsg{err: err}
		}
		return sftpScanDoneMsg{label: label, jobs: jobs, total: total}
	}
}

// ---- delete & mkdir Cmds ----------------------------------------------------

func deleteLocalCmd(panel int, cwd, target string) tea.Cmd {
	return func() tea.Msg {
		err := os.RemoveAll(target)
		return sftpDeletedMsg{panel: panel, cwd: cwd, err: err}
	}
}

func deleteRemoteCmd(client *sftp.Client, panel int, cwd, target string) tea.Cmd {
	return func() tea.Msg {
		stat, err := client.Stat(target)
		if err != nil {
			return sftpDeletedMsg{panel: panel, cwd: cwd, err: err}
		}
		if stat.IsDir() {
			err = deleteRemoteTree(client, target)
		} else {
			err = client.Remove(target)
		}
		return sftpDeletedMsg{panel: panel, cwd: cwd, err: err}
	}
}

func deleteRemoteTree(client *sftp.Client, root string) error {
	walker := client.Walk(root)
	var dirs []string
	for walker.Step() {
		if walker.Err() != nil {
			continue
		}
		if walker.Stat().IsDir() {
			dirs = append(dirs, walker.Path())
		} else {
			client.Remove(walker.Path())
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		client.RemoveDirectory(dirs[i])
	}
	return nil
}

func mkdirLocalCmd(panel int, cwd, name string) tea.Cmd {
	return func() tea.Msg {
		err := os.MkdirAll(filepath.Join(cwd, name), 0755)
		return sftpMkdirDoneMsg{panel: panel, cwd: cwd, err: err}
	}
}

func mkdirRemoteCmd(client *sftp.Client, panel int, cwd, name string) tea.Cmd {
	return func() tea.Msg {
		err := client.MkdirAll(path.Join(cwd, name))
		return sftpMkdirDoneMsg{panel: panel, cwd: cwd, err: err}
	}
}

// ---- transfer Cmds ----------------------------------------------------------

func startDownload(id int, client *sftp.Client, remotePath, localPath string) tea.Cmd {
	return func() tea.Msg {
		stat, err := client.Stat(remotePath)
		if err != nil {
			return sftpXferDoneMsg{id: id, err: err}
		}
		total := stat.Size()
		if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
			return sftpXferDoneMsg{id: id, err: err}
		}
		src, err := client.Open(remotePath)
		if err != nil {
			return sftpXferDoneMsg{id: id, err: err}
		}
		dst, err := os.Create(localPath)
		if err != nil {
			src.Close()
			return sftpXferDoneMsg{id: id, err: err}
		}
		buf := make([]byte, 32*1024)
		var done int64
		var step tea.Cmd
		step = func() tea.Msg {
			n, err := src.Read(buf)
			if n > 0 {
				dst.Write(buf[:n])
				done += int64(n)
			}
			if err != nil {
				src.Close()
				dst.Close()
				if err == io.EOF {
					return sftpXferDoneMsg{id: id}
				}
				return sftpXferDoneMsg{id: id, err: err}
			}
			return sftpProgressMsg{id: id, done: done, total: total, next: step}
		}
		return sftpProgressMsg{id: id, done: 0, total: total, next: step}
	}
}

func startUpload(id int, client *sftp.Client, localPath, remotePath string) tea.Cmd {
	return func() tea.Msg {
		info, err := os.Stat(localPath)
		if err != nil {
			return sftpXferDoneMsg{id: id, err: err}
		}
		total := info.Size()
		remoteDir := path.Dir(remotePath)
		if err := client.MkdirAll(remoteDir); err != nil {
			return sftpXferDoneMsg{id: id, err: err}
		}
		src, err := os.Open(localPath)
		if err != nil {
			return sftpXferDoneMsg{id: id, err: err}
		}
		dst, err := client.Create(remotePath)
		if err != nil {
			src.Close()
			return sftpXferDoneMsg{id: id, err: err}
		}
		buf := make([]byte, 32*1024)
		var done int64
		var step tea.Cmd
		step = func() tea.Msg {
			n, err := src.Read(buf)
			if n > 0 {
				dst.Write(buf[:n])
				done += int64(n)
			}
			if err != nil {
				src.Close()
				dst.Close()
				if err == io.EOF {
					return sftpXferDoneMsg{id: id}
				}
				return sftpXferDoneMsg{id: id, err: err}
			}
			return sftpProgressMsg{id: id, done: done, total: total, next: step}
		}
		return sftpProgressMsg{id: id, done: 0, total: total, next: step}
	}
}

// ---- view -------------------------------------------------------------------

func (m *sftpModal) view(totalW, totalH int) string {
	mw := totalW * 4 / 5
	if mw < 72 {
		mw = 72
	}
	if mw > totalW {
		mw = totalW
	}
	mh := totalH * 3 / 4
	if mh < 18 {
		mh = 18
	}
	if mh > totalH {
		mh = totalH
	}

	// Inner dimensions (border takes 2 rows, 2 cols)
	innerW := mw - 2
	innerH := mh - 2

	// Split: left panel | divider | right panel
	leftW := (innerW - 1) / 2
	rightW := innerW - 1 - leftW

	// Row budget: path bar + separator + list + horiz-divider + hints [+ action row]
	actionRows := 0
	if m.confirm != confirmNone || m.mkdirMode || m.mkdirErr != "" {
		actionRows = 1
	}
	listH := innerH - 2 - 1 - 1 - actionRows // path + sep + hdiv + hints
	if listH < 3 {
		listH = 3
	}

	m.local.scrollToView(listH)
	m.remote.scrollToView(listH)

	leftStr := m.renderPanel(&m.local, leftW, listH, m.activePanel == 0)
	rightStr := m.renderPanel(&m.remote, rightW, listH, m.activePanel == 1)

	divLines := strings.Repeat("│\n", listH+2)
	divLines = divLines[:len(divLines)-1] // trim trailing \n
	panels := lipgloss.JoinHorizontal(lipgloss.Top,
		leftStr,
		styleSftpDiv.Render(divLines),
		rightStr,
	)

	hdiv := styleSftpSep.Render(strings.Repeat("─", innerW))
	hints := m.renderHints(innerW)

	parts := []string{panels, hdiv, hints}
	if m.mkdirMode {
		parts = append(parts, m.renderMkdir(innerW))
	} else if m.mkdirErr != "" {
		parts = append(parts, styleTunnelError.Width(innerW).Render("  ✕ mkdir: "+m.mkdirErr))
	} else if m.confirm != confirmNone {
		parts = append(parts, m.renderConfirm(innerW))
	}

	content := strings.Join(parts, "\n")
	return styleSftpModal.Width(innerW).Height(innerH).Render(content)
}

func (m *sftpModal) renderPanel(p *sftpPanel, w, listH int, active bool) string {
	// Path bar row
	var pathBar string
	if p.editing {
		p.pathInput.Width = w - 2
		pathBar = styleSftpPathActive.Width(w).Render(p.pathInput.View())
	} else {
		label := shortenPath(p.cwd, w-2)
		if active {
			pathBar = styleSftpPathActive.Width(w).Render(" " + label)
		} else {
			pathBar = styleSftpPath.Width(w).Render(" " + label)
		}
	}
	sep := strings.Repeat("─", w)

	// File list
	rows := make([]string, listH)
	for i := 0; i < listH; i++ {
		idx := p.offset + i
		if idx >= len(p.entries) {
			rows[i] = strings.Repeat(" ", w)
			continue
		}
		e := p.entries[idx]
		name := e.name
		if e.isDir {
			name += "/"
		}
		sizeStr := ""
		if !e.isDir {
			sizeStr = formatBytes(e.size)
		}
		rightStr := sizeStr
		if m.showTime {
			tStr := "            " // 12-space placeholder
			if !e.modTime.IsZero() {
				tStr = e.modTime.Format("Jan 02 15:04")
			}
			if sizeStr != "" {
				rightStr = tStr + "  " + sizeStr
			} else {
				rightStr = tStr
			}
		}
		maxName := w - len(rightStr)
		if maxName < 1 {
			maxName = 1
		}
		if len([]rune(name)) > maxName {
			runes := []rune(name)
			name = string(runes[:maxName-1]) + "…"
		}
		line := fmt.Sprintf("%-*s%s", maxName, name, rightStr)

		switch {
		case idx == p.cursor && active:
			rows[i] = styleSftpSelActive.Width(w).Render(line)
		case idx == p.cursor:
			rows[i] = styleSftpSel.Width(w).Render(line)
		case e.isDir:
			rows[i] = styleSftpDir.Width(w).Render(line)
		default:
			rows[i] = styleSftpFile.Width(w).Render(line)
		}
	}

	// If editing, overlay completions on top of file list
	if p.editing && len(p.completions) > 0 {
		for i, comp := range p.completions {
			if i >= listH {
				break
			}
			label := shortenPath(comp, w-2)
			style := styleSftpCompItem
			if i == p.compIdx {
				style = styleSftpCompSel
			}
			rows[i] = style.Width(w).Render(" " + label)
		}
	}

	return strings.Join(append([]string{pathBar, sep}, rows...), "\n")
}

func (m *sftpModal) renderHints(w int) string {
	var hint string
	switch {
	case m.mkdirMode:
		hint = "enter confirm  esc cancel"
	case m.ap().editing:
		hint = "enter navigate  tab complete  esc cancel"
	case m.confirm != confirmNone:
		hint = "[y] yes  [n] cancel"
	default:
		hint = "h/l panel  j/k move  enter open  backspace back  s sync  m mkdir  d delete  T time  ctrl+l path  r refresh  esc close"
	}
	if len(hint) > w {
		hint = hint[:w]
	}
	return styleHelp.Width(w).Render(hint)
}

func (m *sftpModal) renderConfirm(w int) string {
	var text string
	switch m.confirm {
	case confirmScanning:
		text = fmt.Sprintf("↔ sync %s  scanning…  [n] cancel", m.confirmLabel)
	case confirmReady:
		text = fmt.Sprintf("↔ sync %s  %s  [y] yes  [n] cancel", m.confirmLabel, m.confirmMsg)
	case confirmScanErr:
		text = fmt.Sprintf("error: %s  [n] dismiss", m.confirmMsg)
	case confirmDelete:
		text = fmt.Sprintf("Delete %q?  [y] yes  [n] cancel", m.deleteName)
	}
	return styleSftpConfirm.Width(w).Render(text)
}

func (m *sftpModal) renderMkdir(w int) string {
	m.mkdirInput.Width = w - 10
	return styleSftpPathActive.Width(w).Render("  mkdir: " + m.mkdirInput.View())
}

// ---- helpers ----------------------------------------------------------------

func formatBytes(b int64) string {
	const k, m, g = 1024, 1024 * 1024, 1024 * 1024 * 1024
	switch {
	case b >= g:
		return fmt.Sprintf("%.1fG", float64(b)/g)
	case b >= m:
		return fmt.Sprintf("%.1fM", float64(b)/m)
	case b >= k:
		return fmt.Sprintf("%.1fK", float64(b)/k)
	default:
		return fmt.Sprintf("%dB", b)
	}
}

func shortenPath(p string, maxLen int) string {
	if len(p) <= maxLen {
		return p
	}
	if maxLen < 3 {
		return p[:maxLen]
	}
	return "…" + p[len(p)-(maxLen-1):]
}

// openSftpCmd opens an SFTP subsystem on an existing SSH connection.
func openSftpCmd(paneID int, sshClient *gossh.Client) tea.Cmd {
	return func() tea.Msg {
		c, err := sftp.NewClient(sshClient)
		return sftpOpenedMsg{paneID: paneID, client: c, err: err}
	}
}
