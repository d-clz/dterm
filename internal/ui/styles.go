package ui

import "github.com/charmbracelet/lipgloss"

var (
	subtle    = lipgloss.AdaptiveColor{Light: "#D9DCCF", Dark: "#383838"}
	highlight = lipgloss.AdaptiveColor{Light: "#874BFD", Dark: "#7D56F4"}
	special   = lipgloss.AdaptiveColor{Light: "#43BF6D", Dark: "#73F59F"}
	dimText   = lipgloss.AdaptiveColor{Light: "#9B9B9B", Dark: "#5C5C5C"}

	// body styles omit the top border — the title line is rendered separately
	stylePaneBody = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder(), false, true, true, true).
			BorderForeground(subtle)

	stylePaneBodyActive = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder(), false, true, true, true).
				BorderForeground(highlight)

	styleStatusBar = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FFF")).
			Background(lipgloss.Color("#333")).
			Padding(0, 1)

	styleTimestamp = lipgloss.NewStyle().
			Foreground(dimText)

	styleBookmarkName = lipgloss.NewStyle().
				Foreground(special).
				Bold(true)

	styleBookmarkHost = lipgloss.NewStyle().
				Foreground(lipgloss.AdaptiveColor{Light: "#666", Dark: "#999"})

	styleBookmarkSelected = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#FFF")).
				Background(highlight).
				Padding(0, 1)

	styleHelp = lipgloss.NewStyle().
			Foreground(dimText).
			Padding(0, 1)

	// ---- SFTP modal ---------------------------------------------------------

	styleSftpModal = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(highlight)

	styleSftpPath = lipgloss.NewStyle().
			Foreground(dimText)

	styleSftpPathActive = lipgloss.NewStyle().
				Foreground(highlight).
				Bold(true)

	styleSftpDir = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#5555FF", Dark: "#7799FF"}).
			Bold(true)

	styleSftpFile = lipgloss.NewStyle()

	styleSftpSelActive = lipgloss.NewStyle().
				Background(highlight).
				Foreground(lipgloss.Color("#FFF")).
				Bold(true)

	styleSftpSel = lipgloss.NewStyle().
			Background(lipgloss.Color("#555")).
			Foreground(lipgloss.Color("#FFF"))

	styleSftpDiv = lipgloss.NewStyle().
			Foreground(subtle)

	styleSftpSep = lipgloss.NewStyle().
			Foreground(subtle)

	styleSftpConfirm = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#FFD700")).
				Bold(true)

	styleSftpCompItem = lipgloss.NewStyle().
				Background(lipgloss.Color("#333")).
				Foreground(lipgloss.Color("#CCC"))

	styleSftpCompSel = lipgloss.NewStyle().
				Background(lipgloss.Color("#555")).
				Foreground(lipgloss.Color("#FFF")).
				Bold(true)

	// ---- tunnel modal -------------------------------------------------------

	styleTunnelActive = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#73F59F")).
				Bold(true)

	styleTunnelError = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#FF5555")).
				Bold(true)

	styleTunnelRow = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#333", Dark: "#CCC"})

	styleTunnelRowErr = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#FF5555"))

	styleTunnelBadge = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#73F59F")).
				Bold(true)
)
