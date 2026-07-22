package ui

import (
	"fmt"
	"strings"

	"dterm/internal/config"
)

const (
	viewModeAll   = 0
	viewModeGroup = 1
)

// displayRow is one rendered line in the bookmark list area.
type displayRow struct {
	header bool
	group  string
	visIdx int // index into bv.visible (-1 for headers)
}

// BookmarkView is the bookmark home screen model.
type BookmarkView struct {
	bookmarks []config.Bookmark
	visible   []int    // bookmark indices in current display order
	cursor    int      // index into visible
	filter    string   // current live-filter query (empty = show all)
	searching bool     // "/" mode: user is typing a query
	viewMode  int      // viewModeAll or viewModeGroup
	groups    []string // sorted group names (for group mode)
	groupIdx  int      // index into groups (group mode)
	width     int
	height    int
}

func NewBookmarkView(bookmarks []config.Bookmark) BookmarkView {
	bv := BookmarkView{bookmarks: bookmarks}
	bv.rebuildGroups()
	bv.rebuildVisible()
	return bv
}

// rebuildGroups recomputes the sorted groups slice from bookmarks.
func (bv *BookmarkView) rebuildGroups() {
	seen := map[string]bool{}
	bv.groups = bv.groups[:0]
	for _, bm := range bv.bookmarks {
		if !seen[bm.Group] {
			seen[bm.Group] = true
			bv.groups = append(bv.groups, bm.Group)
		}
	}
	sortGroups(bv.groups)
	if bv.groupIdx >= len(bv.groups) {
		bv.groupIdx = 0
	}
}

// rebuildVisible recomputes visible and clamps the cursor.
func (bv *BookmarkView) rebuildVisible() {
	if bv.filter != "" {
		// Filter mode: flat search across all bookmarks.
		q := strings.ToLower(bv.filter)
		bv.visible = bv.visible[:0]
		for i, bm := range bv.bookmarks {
			if strings.Contains(strings.ToLower(bm.Name), q) ||
				strings.Contains(strings.ToLower(bm.Host), q) ||
				strings.Contains(strings.ToLower(bm.User), q) {
				bv.visible = append(bv.visible, i)
			}
		}
	} else if bv.viewMode == viewModeGroup && len(bv.groups) > 0 {
		// Group mode: show only the current group's bookmarks.
		g := bv.groups[bv.groupIdx]
		bv.visible = bv.visible[:0]
		for i, bm := range bv.bookmarks {
			if bm.Group == g {
				bv.visible = append(bv.visible, i)
			}
		}
	} else {
		// All mode: flat list in original YAML order.
		bv.visible = make([]int, len(bv.bookmarks))
		for i := range bv.bookmarks {
			bv.visible[i] = i
		}
	}
	if bv.cursor >= len(bv.visible) {
		bv.cursor = len(bv.visible) - 1
	}
	if bv.cursor < 0 {
		bv.cursor = 0
	}
}

func sortGroups(gs []string) {
	// bubble sort is fine for a handful of groups; empty string sorts first
	for i := 0; i < len(gs); i++ {
		for j := i + 1; j < len(gs); j++ {
			a, b := gs[i], gs[j]
			less := false
			if a == "" {
				less = true
			} else if b != "" {
				less = a < b
			}
			if !less {
				gs[i], gs[j] = gs[j], gs[i]
			}
		}
	}
}

// computeDisplayRows returns the flat list of rendered rows for the list area.
func (bv *BookmarkView) computeDisplayRows() []displayRow {
	rows := make([]displayRow, len(bv.visible))
	for i := range bv.visible {
		rows[i] = displayRow{visIdx: i}
	}
	return rows
}

// ---- search -----------------------------------------------------------------

func (bv *BookmarkView) StartSearch() {
	bv.searching = true
	bv.filter = ""
	bv.rebuildVisible()
}

func (bv *BookmarkView) AppendSearch(r rune) {
	bv.filter += string(r)
	bv.cursor = 0
	bv.rebuildVisible()
}

func (bv *BookmarkView) BackspaceSearch() {
	if len(bv.filter) > 0 {
		runes := []rune(bv.filter)
		bv.filter = string(runes[:len(runes)-1])
		bv.cursor = 0
		bv.rebuildVisible()
	}
}

func (bv *BookmarkView) ConfirmSearch() {
	bv.searching = false
	// keep filter active so the list stays filtered until esc
}

func (bv *BookmarkView) CancelSearch() {
	bv.searching = false
	bv.filter = ""
	bv.rebuildVisible()
}

// ---- view mode --------------------------------------------------------------

func (bv *BookmarkView) ToggleViewMode() {
	if bv.viewMode == viewModeAll {
		bv.viewMode = viewModeGroup
	} else {
		bv.viewMode = viewModeAll
	}
	bv.cursor = 0
	bv.rebuildVisible()
}

func (bv *BookmarkView) NextGroup() {
	if len(bv.groups) == 0 {
		return
	}
	bv.groupIdx = (bv.groupIdx + 1) % len(bv.groups)
	bv.cursor = 0
	bv.rebuildVisible()
}

func (bv *BookmarkView) PrevGroup() {
	if len(bv.groups) == 0 {
		return
	}
	bv.groupIdx = (bv.groupIdx - 1 + len(bv.groups)) % len(bv.groups)
	bv.cursor = 0
	bv.rebuildVisible()
}

// ---- navigation -------------------------------------------------------------

func (bv *BookmarkView) Up() {
	if bv.cursor > 0 {
		bv.cursor--
	}
}

func (bv *BookmarkView) Down() {
	if bv.cursor < len(bv.visible)-1 {
		bv.cursor++
	}
}

func (bv *BookmarkView) Selected() *config.Bookmark {
	if bv.cursor >= 0 && bv.cursor < len(bv.visible) {
		return &bv.bookmarks[bv.visible[bv.cursor]]
	}
	return nil
}

// bookmarkListOffset is rows emitted before the first list row:
// blank + 4 art lines + blank + header + separator = 8
const bookmarkListOffset = 8

func (bv *BookmarkView) ClickAt(y int) {
	offset := bookmarkListOffset
	if bv.viewMode == viewModeGroup && bv.filter == "" && !bv.searching {
		offset++ // group strip adds one row between separator and list
	}
	listY := y - offset
	if listY < 0 {
		return
	}
	rows := bv.computeDisplayRows()
	if listY >= len(rows) {
		return
	}
	row := rows[listY]
	if !row.header && row.visIdx >= 0 {
		bv.cursor = row.visIdx
	}
}

func (bv *BookmarkView) Resize(w, h int) {
	bv.width = w
	bv.height = h
}

// ---- view -------------------------------------------------------------------

func (bv *BookmarkView) groupName(g string) string {
	if g == "" {
		return "ungrouped"
	}
	return g
}

func (bv *BookmarkView) renderGroupStrip() string {
	n := len(bv.groups)
	if n == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("  ")
	if bv.groupIdx > 0 {
		prev := bv.groupName(bv.groups[bv.groupIdx-1])
		sb.WriteString(styleHelp.Render("← " + prev + "  "))
	}
	cur := bv.groupName(bv.groups[bv.groupIdx])
	sb.WriteString(styleBookmarkSelected.Render(" " + cur + " "))
	if bv.groupIdx < n-1 {
		next := bv.groupName(bv.groups[bv.groupIdx+1])
		sb.WriteString(styleHelp.Render("  " + next + " →"))
	}
	return sb.String()
}

func (bv *BookmarkView) View() string {
	var b strings.Builder

	title := `
    __  __                                    
   /\ \/\ \__                                 
   \_\ \ \ ,_\    __   _ __    ___ ___        
   /'_  \ \ \/  /'__ \/\ '__\/' __' __ \      
  /\ \L\ \ \ \_/\  __/\ \ \/ /\ \/\ \/\ \  __
  \ \___,_\ \__\ \____\\ \_\ \ \_\ \_\ \_\/\_\
   \/__,_ /\/__/\/____/ \/_/  \/_/\/_/\/_/\/_/

`
	b.WriteString(title)
	b.WriteString("\n")

	if len(bv.bookmarks) == 0 {
		b.WriteString("  No bookmarks found.\n")
		b.WriteString("  Add entries to ~/.dterm/bookmarks.yaml\n\n")
		b.WriteString("  Example:\n")
		b.WriteString("  bookmarks:\n")
		b.WriteString("    - name: my-server\n")
		b.WriteString("      host: 192.168.1.10\n")
		b.WriteString("      user: root\n")
		b.WriteString("      key_file: ~/.ssh/id_rsa\n")
		return b.String()
	}

	// Header row
	b.WriteString(fmt.Sprintf("  %-20s %-30s %-10s %s\n", "NAME", "HOST", "PORT", "USER"))
	b.WriteString("  " + strings.Repeat("─", 70) + "\n")

	// Group strip (group mode only, not while filtering)
	if bv.viewMode == viewModeGroup && bv.filter == "" && !bv.searching {
		b.WriteString(bv.renderGroupStrip() + "\n")
	}

	rows := bv.computeDisplayRows()
	for _, row := range rows {
		if row.visIdx < 0 || row.visIdx >= len(bv.visible) {
			continue
		}
		bm := bv.bookmarks[bv.visible[row.visIdx]]
		port := fmt.Sprintf("%d", bm.Port)
		if row.visIdx == bv.cursor {
			line := fmt.Sprintf("  %-20s %-30s %-10s %s", bm.Name, bm.Host, port, bm.User)
			b.WriteString(styleBookmarkSelected.Render(line) + "\n")
		} else {
			name := styleBookmarkName.Render(fmt.Sprintf("%-20s", bm.Name))
			rest := styleBookmarkHost.Render(fmt.Sprintf("%-30s %-10s %s", bm.Host, port, bm.User))
			b.WriteString("  " + name + rest + "\n")
		}
	}

	if len(bv.visible) == 0 && bv.filter != "" {
		b.WriteString(styleHelp.Render("  no matches") + "\n")
	}

	b.WriteString("\n")

	if bv.searching {
		b.WriteString(styleBookmarkName.Render("  /"+bv.filter+"▌") + "\n")
		b.WriteString(styleHelp.Render("  type to filter • enter confirm • esc cancel"))
	} else if bv.filter != "" {
		b.WriteString(styleBookmarkHost.Render("  filter: "+bv.filter) + "  " + styleHelp.Render("esc clear") + "\n")
		b.WriteString(styleHelp.Render("  ↑/↓ navigate • enter connect • / new search • q quit"))
	} else if bv.viewMode == viewModeGroup {
		b.WriteString(styleHelp.Render("  ↑/↓ navigate • enter connect • h/l prev/next group • g all view • / search • q quit"))
	} else {
		b.WriteString(styleHelp.Render("  ↑/↓ navigate • enter connect • g group view • / search • q quit"))
	}

	return b.String()
}
