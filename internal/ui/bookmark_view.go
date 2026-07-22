package ui

import (
	"fmt"
	"strings"

	"dterm/internal/config"
)

type BookmarkView struct {
	bookmarks []config.Bookmark
	cursor    int
	width     int
	height    int
}

func NewBookmarkView(bookmarks []config.Bookmark) BookmarkView {
	return BookmarkView{
		bookmarks: bookmarks,
		cursor:    0,
	}
}

func (bv *BookmarkView) Up() {
	if bv.cursor > 0 {
		bv.cursor--
	}
}

func (bv *BookmarkView) Down() {
	if bv.cursor < len(bv.bookmarks)-1 {
		bv.cursor++
	}
}

func (bv *BookmarkView) Selected() *config.Bookmark {
	if bv.cursor >= 0 && bv.cursor < len(bv.bookmarks) {
		return &bv.bookmarks[bv.cursor]
	}
	return nil
}

// bookmarkListOffset is the number of rows View() emits before the first bookmark entry:
// blank + 4 art lines + blank + header + separator = 8
const bookmarkListOffset = 8

func (bv *BookmarkView) ClickAt(y int) {
	idx := y - bookmarkListOffset
	if idx >= 0 && idx < len(bv.bookmarks) {
		bv.cursor = idx
	}
}

func (bv *BookmarkView) Resize(w, h int) {
	bv.width = w
	bv.height = h
}

func (bv *BookmarkView) View() string {
	var b strings.Builder

	title := `
   ___  ____  ____ __  
  / _ \/ __ \/ __// / 
 / , _/ /_/ /\ \ / _ \
/_/|_|\____/___//_//_/
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

	// header
	b.WriteString(fmt.Sprintf("  %-20s %-30s %-10s %s\n", "NAME", "HOST", "PORT", "USER"))
	b.WriteString("  " + strings.Repeat("─", 70) + "\n")

	for i, bm := range bv.bookmarks {
		port := fmt.Sprintf("%d", bm.Port)
		line := fmt.Sprintf("  %-20s %-30s %-10s %s", bm.Name, bm.Host, port, bm.User)

		if i == bv.cursor {
			line = styleBookmarkSelected.Render(line)
		} else {
			name := styleBookmarkName.Render(fmt.Sprintf("%-20s", bm.Name))
			host := styleBookmarkHost.Render(fmt.Sprintf("%-30s %-10s %s", bm.Host, port, bm.User))
			line = "  " + name + host
		}
		b.WriteString(line + "\n")
	}

	b.WriteString("\n")
	b.WriteString(styleHelp.Render("↑/↓ navigate • enter connect • q quit"))

	return b.String()
}
