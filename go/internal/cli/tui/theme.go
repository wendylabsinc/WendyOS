package tui

import "github.com/charmbracelet/lipgloss"

// Emerald color palette.
const (
	Emerald50  = lipgloss.Color("#ecfdf5")
	Emerald100 = lipgloss.Color("#d1fae5")
	Emerald200 = lipgloss.Color("#a7f3d0")
	Emerald300 = lipgloss.Color("#6ee7b7")
	Emerald400 = lipgloss.Color("#34d399")
	Emerald500 = lipgloss.Color("#10b981")
	Emerald600 = lipgloss.Color("#059669")
	Emerald700 = lipgloss.Color("#047857")
	Emerald800 = lipgloss.Color("#065f46")
	Emerald900 = lipgloss.Color("#064e3b")
	Emerald950 = lipgloss.Color("#022c22")

	// Amber color palette.
	Amber500 = lipgloss.Color("#f59e0b")

	// Semantic aliases used across TUI components.
	ColorPrimary    = Emerald400            // titles, spinners, scanning text
	ColorAccent     = Emerald500            // progress bar, active indicators
	ColorHeaderFg   = Emerald50             // table header foreground
	ColorHeaderBg   = Emerald800            // table header background
	ColorBorder     = Emerald600            // table borders
	ColorSelectedBg = Emerald900            // table selection background
	ColorSelectedFg = Emerald100            // table selection foreground
	ColorDim        = lipgloss.Color("240") // muted/hint text (neutral gray)
	ColorNotice     = Amber500              // informational notices
)
