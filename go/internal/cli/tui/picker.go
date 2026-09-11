package tui

import (
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	bubbleTable "github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// displayAddress strips a trailing numeric port from an address for display in
// the device list — the agent port (50051/50052) is an implementation detail and
// the same for every device. The full host:port is still used to connect and is
// copied to the clipboard. Values that aren't host:<number> (BLE UUIDs,
// "provider: id") are returned unchanged.
func displayAddress(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if _, err := strconv.Atoi(port); err != nil {
		return addr
	}
	return host
}

// newProbeSpinner builds the spinner that animates the Agent/OS columns while a
// device probe is in flight. Its Style is intentionally empty so View() returns
// a bare frame rune: the frame is written into plain table cells, which the
// bubbles table truncates without ANSI awareness, so it must carry no escapes.
func newProbeSpinner() spinner.Model {
	return spinner.New(spinner.WithSpinner(spinner.Dot))
}

// ProbeState describes whether an agent version/OS probe for a device row is in
// flight, succeeded, or failed. The zero value (ProbeNone) renders the version
// columns exactly as before, so non-probing pickers are unaffected.
type ProbeState int

const (
	ProbeNone    ProbeState = iota // no probe semantics: show version text as-is
	ProbePending                   // probe in flight: show an animated spinner
	ProbeOK                        // probe succeeded: show the version
	ProbeFailed                    // probe failed: show the error glyph
	ProbeOffline                   // device offline: show "offline" text
)

// ProbeFailedGlyph marks a row whose agent probe failed and for which no version
// is cached. It is rendered red by the view layer (see ColorizeProbeGlyphs); the
// cell value itself is kept as a plain glyph so the table's non-ANSI-aware
// truncation stays correct.
const ProbeFailedGlyph = "▲"

// probeFailedColored wraps the failure glyph in a red foreground that resets
// only the foreground (\x1b[39m), not the whole SGR state. A full reset would
// clear a selected row's background for the rest of the line; resetting just the
// foreground keeps the highlight intact. 196 matches the red used elsewhere in
// the device tables.
const probeFailedColored = "\x1b[38;5;196m" + ProbeFailedGlyph + "\x1b[39m"

// ColorizeProbeGlyphs colors the (plain-text) failure glyph red in an
// already-rendered table view. The glyph is kept plain inside table cells so the
// table's non-ANSI-aware truncation stays correct; color is applied here, after
// layout, so it never affects column widths.
func ColorizeProbeGlyphs(view string) string {
	return strings.ReplaceAll(view, ProbeFailedGlyph, probeFailedColored)
}

// probeColumnValue renders an Agent/OS cell for the given probe state. While
// pending it shows the current spinner frame; on failure with no cached version
// it shows the error glyph; for offline it shows "offline"; otherwise it shows
// the version text (a cached version is preserved even after a later transient failure).
func probeColumnValue(state ProbeState, version, frame string) string {
	switch state {
	case ProbePending:
		return frame
	case ProbeFailed:
		if version == "" {
			return ProbeFailedGlyph
		}
	case ProbeOffline:
		return "offline"
	}
	return version
}

// formatOSNameVersion joins an OS/distro name and its version into a single
// display string (e.g. "ubuntu" + "24.04" -> "ubuntu 24.04"). Either part may
// be empty; the result never has leading or trailing space.
//
// When the version string already begins with the OS name (case-insensitive),
// the name is dropped to avoid a redundant prefix. WendyOS reports the distro
// ID "wendyos" alongside a version like "WendyOS-0.16.2", which would otherwise
// render as the ugly "wendyos WendyOS-0.16.2"; here it becomes "WendyOS-0.16.2".
func formatOSNameVersion(os, version string) string {
	os = strings.TrimSpace(os)
	version = strings.TrimSpace(version)
	if os != "" && version != "" && len(version) >= len(os) && strings.EqualFold(version[:len(os)], os) {
		return version
	}
	return strings.TrimSpace(os + " " + version)
}

// PickerItem represents a selectable row in the device picker.
type PickerItem struct {
	// Display columns rendered in the table.
	Name         string
	Description  string // optional secondary text rendered dimmed
	Type         string // "LAN", "BLE", "External", etc.
	Size         string // optional picker-specific metadata column
	Parameters   string // optional picker-specific metadata column
	Comments     string // optional picker-specific metadata column
	USB          string // non-empty when the device is connected over USB
	Address      string
	AgentVersion string
	OS           string // distro/OS name (e.g. "ubuntu"); shown alongside OSVersion
	OSVersion    string
	Provisioned  string // "Provisioned" or "Unprovisioned" when known, empty otherwise
	Hint         string // optional footer text shown when this item is highlighted

	// Probe reflects whether the agent version/OS probe for this row is in
	// flight (ProbePending), done (ProbeOK), or failed (ProbeFailed). The zero
	// value (ProbeNone) renders the version columns verbatim.
	Probe ProbeState
	// ProbeFrame is the spinner frame shown in the Agent/OS columns while
	// Probe == ProbePending. The owning model refreshes it on every spinner
	// tick; it is ignored for every other probe state.
	ProbeFrame string

	// Section, when non-empty, groups this item under a non-selectable header
	// row bearing the section name. Sections are rendered in the order they
	// first appear after sorting, so callers control grouping order via SortKey.
	// When no visible item sets Section, the picker renders and navigates
	// exactly as it does without sections.
	Section string

	// DedupKey is used for deduplication. If empty, Name is used.
	// Items with the same DedupKey (case-insensitive) are merged via MergeItem.
	DedupKey string

	// Supersedes, when set, is the DedupKey of a row this item replaces. The
	// existing row is dropped before this one is added, so a provisional
	// identity can be retired by the real one without the two colliding on a
	// shared key.
	Supersedes string

	// DefaultKeys are optional alternate identities compared against
	// PickerModel.DefaultKey when rendering the default marker.
	DefaultKeys []string

	// SortKey overrides the sort order when set. Items are sorted by SortKey
	// first (when non-empty), then by name. Use this to pin items to a specific
	// position (e.g. "0_first", "z_last") without affecting the display name.
	SortKey string

	// Insecure is true when the device is reachable but the connection is not
	// secured with mTLS. A warning is shown in the picker when this item is highlighted.
	Insecure bool

	// AgentOutdated marks the agent as older than the CLI, rendered as a glyph
	// on the Agent column. Callers own the comparison; the picker only displays it.
	AgentOutdated bool

	// Value is the opaque payload returned when this item is selected.
	Value interface{}
}

// PickerAddMsg adds new items to the picker. Duplicates (by DedupKey, or Name
// if DedupKey is empty) are merged via MergeItem or silently dropped.
type PickerAddMsg struct {
	Items []PickerItem
}

// PickerDoneMsg signals that discovery has finished. The picker remains
// interactive so the user can still select from the collected items.
type PickerDoneMsg struct{}

// PickerSetMsg replaces all items in the picker. It is intended for
// authoritative refreshes where missing items should disappear from the list.
type PickerSetMsg struct {
	Items []PickerItem
}

// PickerRemoveMsg drops the item whose DedupKey (or Name, when it has none)
// matches Key, case-insensitively like every other key comparison here. It is
// how a discovery source takes a row back -- a LAN sighting that turned out to
// be a local VM. An unknown key is a no-op.
type PickerRemoveMsg struct {
	Key string
}

// PickerModel is a Bubble Tea model that presents a live-updating list of
// items and lets the user select one with arrow keys + Enter.
type PickerModel struct {
	Title string // header line, e.g. "Select a device"

	// MergeItem is called when a new item shares a DedupKey with an existing
	// item. The caller can update existing in place (type, address, value, ...).
	// If nil, duplicate items are silently dropped.
	MergeItem func(existing *PickerItem, incoming PickerItem)

	// OnSetDefault is called when the user presses 'd' on the highlighted item.
	// The return value is shown as a flash confirmation; return "" for no message.
	// If nil, 'd' is ignored.
	OnSetDefault func(item PickerItem) string

	// OnUnsetDefault is called when the user presses 'x'.
	// The return value is shown as a flash confirmation; return "" for no message.
	// If nil, 'x' is ignored.
	OnUnsetDefault func() string

	// OnRemoveItem is called when the user presses 'r' on the highlighted item.
	// Returns (flash message, isError, replacement).
	// If replacement is non-nil, the row is updated in place with the new item.
	// If replacement is nil and isError is false, the row is removed.
	// If replacement is nil and isError is true, the row is kept unchanged.
	// If nil, 'r' is ignored.
	OnRemoveItem func(item PickerItem) (string, bool, *PickerItem)

	// OnCreateItem is called when the user presses 'c'. Returning quit=true
	// closes the picker, which an action needing the terminal to itself must
	// do: creating a VM downloads an image behind its own progress program,
	// and two Bubble Tea programs cannot share a terminal.
	// If nil, 'c' is ignored.
	OnCreateItem func() (flash string, quit bool)

	// OnStopItem runs asynchronously when the user presses 's'. It must not
	// mutate the model, and should honor its owner's cancellation context.
	// Returns (flash message, isError). If nil, 's' is ignored.
	OnStopItem   func(item PickerItem) (string, bool)
	stoppingName string

	// RemoveHint is the wording for 'r' in the header, for a picker whose rows
	// are not cloud credentials. Empty keeps the default.
	RemoveHint string

	// OnCopyItem is called when the user presses Enter. The callback should
	// perform the copy operation (e.g. write to clipboard) and return a flash
	// confirmation message. When set, Enter does NOT navigate away — the picker
	// stays open so the user can see the confirmation and press q to quit. If
	// nil, Enter selects the item and closes the picker as usual.
	OnCopyItem func(item PickerItem) string

	// flashMessage is the transient status line shown below the table after
	// an action (d/x/r/enter). Cleared on the next navigation key.
	flashMessage string
	flashIsError bool

	// DefaultKey is compared case-insensitively against each item's DedupKey
	// (or Name if DedupKey is empty). Should be stored lowercase for consistency.
	// Shown with a ✦ indicator in the table.
	DefaultKey string

	// Filterable enables find-as-you-type filtering: printable keys narrow
	// the list to items whose Name contains the typed query
	// (case-insensitive), backspace edits the query, and esc clears it (esc
	// and ctrl+c quit when the query is empty). Matched characters are
	// highlighted in the Name column. Enabling this retargets the plain
	// 'q'/'d'/'x' hotkeys into the filter query, so only use it for pickers
	// whose items are free-form text (e.g. WiFi SSIDs).
	Filterable bool

	filter  string
	items   []PickerItem
	seenIdx map[string]int // dedup key -> index in items
	// rowItem maps each table row to its index in the visible-items slice, or
	// -1 when the row is a non-selectable section header. When no item sets a
	// Section this is the identity mapping, so behavior matches the headerless
	// picker exactly.
	rowItem      []int
	table        BubbleTable
	spinner      spinner.Model // animates Agent/OS cells while probes are pending
	columns      []pickerColumnDef
	fixedColumns bool
	deviceLegend bool // render the device glyph legend under the table
	selected     *PickerItem
	scanning     bool
	quitting     bool
	width        int
	height       int
}

// PickerColumn defines a caller-provided picker table column.
type PickerColumn struct {
	Title    string
	MinWidth int
	Required bool
	Value    func(PickerItem) string
}

func NewPicker() PickerModel {
	m := PickerModel{
		Title:        "Select a device",
		seenIdx:      make(map[string]int),
		table:        newPickerTable(),
		spinner:      newProbeSpinner(),
		columns:      pickerDeviceColumnDefs,
		fixedColumns: true,
		deviceLegend: true,
		scanning:     true,
	}
	m.refreshTable()
	return m
}

func NewPickerWithTitle(title string) PickerModel {
	m := PickerModel{
		Title:    title,
		seenIdx:  make(map[string]int),
		table:    newPickerTable(),
		spinner:  newProbeSpinner(),
		scanning: true,
	}
	m.refreshTable()
	return m
}

// NewPickerWithTitleAndColumns creates a picker with a custom title and stable
// caller-provided table columns.
func NewPickerWithTitleAndColumns(title string, columns []PickerColumn) PickerModel {
	if len(columns) == 0 {
		return NewPickerWithTitle(title)
	}
	m := NewPickerWithTitle(title)
	m.columns = pickerColumnDefsFromColumns(columns)
	m.fixedColumns = true
	m.refreshTable()
	return m
}

func (m PickerModel) Init() tea.Cmd { return m.spinner.Tick }

// anyProbePending reports whether any item still has a probe in flight, i.e.
// whether the spinner has anything to animate.
func (m PickerModel) anyProbePending() bool {
	for i := range m.items {
		if m.items[i].Probe == ProbePending {
			return true
		}
	}
	return false
}

type pickerStopResultMsg struct {
	flash   string
	isError bool
}

func (m PickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case pickerStopResultMsg:
		m.stoppingName = ""
		m.flashMessage, m.flashIsError = msg.flash, msg.isError
		m.refreshTable()
		return m, nil
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		// Re-render so pending rows pick up the new frame; skip the rebuild
		// when nothing is animating, but keep the tick loop alive cheaply.
		if m.anyProbePending() {
			m.refreshTable()
		}
		return m, cmd
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		var cmd tea.Cmd
		m.table, cmd = m.table.Update(msg)
		m.refreshTable()
		return m, cmd
	case tea.KeyMsg:
		key := msg.String()
		switch {
		case key == "enter":
			visible := m.visibleItems()
			if idx := m.itemIndexForRow(m.table.Cursor()); idx >= 0 && idx < len(visible) {
				item := visible[idx]
				if m.OnCopyItem != nil {
					m.flashMessage = m.OnCopyItem(item)
					m.flashIsError = false
					return m, nil
				}
				m.selected = &item
				return m, tea.Quit
			}
		case key == "d" && !m.Filterable:
			if m.OnSetDefault != nil {
				visible := m.visibleItems()
				if idx := m.itemIndexForRow(m.table.Cursor()); idx >= 0 && idx < len(visible) {
					item := visible[idx]
					dk := strings.ToLower(item.DedupKey)
					if dk == "" {
						dk = strings.ToLower(item.Name)
					}
					m.DefaultKey = dk
					m.flashMessage = m.OnSetDefault(item)
					m.flashIsError = false
					m.refreshTable()
				}
			}
			return m, nil
		case key == "x" && !m.Filterable:
			if m.OnUnsetDefault != nil {
				m.DefaultKey = ""
				m.flashMessage = m.OnUnsetDefault()
				m.flashIsError = false
				m.refreshTable()
			}
			return m, nil
		case key == "c" && !m.Filterable:
			if m.OnCreateItem != nil {
				flash, quit := m.OnCreateItem()
				m.flashMessage = flash
				m.flashIsError = false
				if quit {
					return m, tea.Quit
				}
			}
			return m, nil
		case key == "s" && !m.Filterable:
			if m.OnStopItem != nil && m.stoppingName == "" {
				visible := m.visibleItems()
				if idx := m.itemIndexForRow(m.table.Cursor()); idx >= 0 && idx < len(visible) {
					item, stop := visible[idx], m.OnStopItem
					m.stoppingName = item.Name
					m.flashMessage = ""
					return m, func() tea.Msg {
						flash, isError := stop(item)
						return pickerStopResultMsg{flash: flash, isError: isError}
					}
				}
			}
			return m, nil
		case key == "r" && !m.Filterable:
			if m.OnRemoveItem != nil {
				visible := m.visibleItems()
				if idx := m.itemIndexForRow(m.table.Cursor()); idx >= 0 && idx < len(visible) {
					item := visible[idx]
					flash, isError, replacement := m.OnRemoveItem(item)
					m.flashMessage = flash
					m.flashIsError = isError
					if replacement != nil {
						// Replace the item in the list (e.g. moved to a new section).
						dedupKey := strings.ToLower(item.DedupKey)
						if dedupKey == "" {
							dedupKey = strings.ToLower(item.Name)
						}
						for i := range m.items {
							k := strings.ToLower(m.items[i].DedupKey)
							if k == "" {
								k = strings.ToLower(m.items[i].Name)
							}
							if k == dedupKey {
								newKey := strings.ToLower(replacement.DedupKey)
								if newKey == "" {
									newKey = strings.ToLower(replacement.Name)
								}
								delete(m.seenIdx, dedupKey)
								m.items[i] = *replacement
								m.seenIdx[newKey] = i
								break
							}
						}
					} else if !isError {
						// No replacement and not an error: remove the row.
						dedupKey := strings.ToLower(item.DedupKey)
						if dedupKey == "" {
							dedupKey = strings.ToLower(item.Name)
						}
						filtered := m.items[:0]
						for _, it := range m.items {
							k := strings.ToLower(it.DedupKey)
							if k == "" {
								k = strings.ToLower(it.Name)
							}
							if k != dedupKey {
								filtered = append(filtered, it)
							}
						}
						m.items = filtered
						delete(m.seenIdx, dedupKey)
					}
					m.refreshTable()
				}
			}
			return m, nil
		case key == "esc" && m.Filterable && m.filter != "":
			m.filter = ""
			m.table.SetCursor(0)
			m.refreshTable()
			return m, nil
		case key == "ctrl+c", key == "esc", key == "q" && !m.Filterable:
			m.quitting = true
			return m, tea.Quit
		case key == "backspace" && m.Filterable:
			if m.filter != "" {
				runes := []rune(m.filter)
				m.filter = string(runes[:len(runes)-1])
				m.table.SetCursor(0)
				m.refreshTable()
			}
			return m, nil
		case (msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace) && m.Filterable:
			// Pasted input can carry control characters; keep them out of
			// the query (it is echoed back to the terminal verbatim), and
			// cap its length so a large paste can't distort the layout.
			if typed := StripControl(string(msg.Runes)); typed != "" {
				const maxFilterLen = 64
				if room := maxFilterLen - len([]rune(m.filter)); room > 0 {
					runes := []rune(typed)
					if len(runes) > room {
						runes = runes[:room]
					}
					m.filter += string(runes)
					m.table.SetCursor(0)
					m.refreshTable()
				}
			}
			return m, nil
		default:
			prev := m.table.Cursor()
			var cmd tea.Cmd
			m.table, cmd = m.table.Update(msg)
			// Skip over non-selectable section headers in the direction of
			// travel so the cursor never rests on one.
			dir := 1
			if m.table.Cursor() < prev {
				dir = -1
			}
			m.snapCursorToSelectable(dir)
			// Clear any flash message when the user navigates.
			if m.table.Cursor() != prev {
				m.flashMessage = ""
			}
			return m, cmd
		}

	case PickerAddMsg:
		changed := false
		// Captured before any supersede removes a row, since currentCursorKey
		// reads the pre-rebuild table against the mutated item slice.
		cursorKey := m.currentCursorKey()
		for _, item := range msg.Items {
			key := strings.ToLower(pickerItemKey(item))
			if superseded := strings.ToLower(item.Supersedes); superseded != "" && superseded != key {
				if m.removeItemByKey(superseded) {
					changed = true
					if cursorKey == superseded {
						// Keep the highlight on the replacement rather than
						// letting it slide onto a neighbour mid-selection.
						cursorKey = key
					}
				}
			}
			if idx, ok := m.seenIdx[key]; ok {
				if m.MergeItem != nil {
					m.MergeItem(&m.items[idx], item)
					changed = true
				}
				continue
			}
			m.seenIdx[key] = len(m.items)
			m.items = append(m.items, item)
			changed = true
		}
		if changed {
			m.refreshTableWithCursorKey(cursorKey)
		}

	case PickerDoneMsg:
		m.scanning = false

	case PickerRemoveMsg:
		cursorKey := m.currentCursorKey()
		if m.removeItemByKey(strings.ToLower(msg.Key)) {
			m.refreshTableWithCursorKey(cursorKey)
		}

	case PickerSetMsg:
		cursorKey := m.currentCursorKey()
		m.items = nil
		m.seenIdx = make(map[string]int, len(msg.Items))
		for _, item := range msg.Items {
			key := strings.ToLower(pickerItemKey(item))
			if idx, ok := m.seenIdx[key]; ok {
				if m.MergeItem != nil {
					m.MergeItem(&m.items[idx], item)
				}
				continue
			}
			m.seenIdx[key] = len(m.items)
			m.items = append(m.items, item)
		}
		m.refreshTableWithCursorKey(cursorKey)
	}

	return m, nil
}

var (
	pickerTitle    = lipgloss.NewStyle().Bold(true).Foreground(ColorPrimary)
	pickerHint     = lipgloss.NewStyle().Foreground(ColorDim)
	pickerScanning = lipgloss.NewStyle().Foreground(ColorPrimary)
	pickerInsecure = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#ef4444"))
)

func (m PickerModel) View() string {
	if m.quitting || m.selected != nil {
		return ""
	}

	var sb strings.Builder

	scrollHint := ""
	if m.canScrollTable() {
		scrollHint = ", ←/→ scroll"
	}
	enterAction := "enter select"
	if m.OnCopyItem != nil {
		enterAction = "enter copy"
	}
	hint := " (↑/↓ navigate" + scrollHint + ", " + enterAction + ", q quit)"
	if m.Filterable {
		hint = " (type to filter, ↑/↓ navigate" + scrollHint + ", " + enterAction + ", esc quit)"
	}
	if m.OnSetDefault != nil || m.OnUnsetDefault != nil || m.OnRemoveItem != nil ||
		m.OnCreateItem != nil || m.OnStopItem != nil {
		extras := ""
		if m.OnSetDefault != nil {
			extras += ", d set default"
		}
		if m.OnUnsetDefault != nil {
			extras += ", x clear default"
		}
		if m.OnCreateItem != nil {
			extras += ", c create"
		}
		if m.OnStopItem != nil {
			extras += ", s stop"
		}
		if m.OnRemoveItem != nil {
			label := m.RemoveHint
			if label == "" {
				label = "remove creds"
			}
			extras += ", r " + label
		}
		hint = " (↑/↓ navigate" + scrollHint + ", " + enterAction + extras + ", q quit)"
	}
	sb.WriteString(m.viewLine(pickerTitle.Render(m.Title)+pickerHint.Render(hint)) + "\n\n")

	if m.Filterable && m.filter != "" {
		// The query is sanitized as it is typed; strip again here so this
		// render site is safe in isolation.
		sb.WriteString(m.viewLine("  Filter: "+StripControl(m.filter)+pickerHint.Render("  (esc to clear)")) + "\n\n")
	}

	if len(m.items) == 0 {
		if m.scanning {
			sb.WriteString(m.viewLine(pickerScanning.Render("  Scanning...")) + "\n")
		} else {
			sb.WriteString(m.viewLine(pickerHint.Render("  No options found.")) + "\n")
		}
		return sb.String()
	}

	visible := m.visibleItems()
	if len(visible) == 0 {
		sb.WriteString(m.viewLine(pickerHint.Render("  No matches — esc to clear the filter.")) + "\n")
		return sb.String()
	}

	sb.WriteString(colorizeSectionHeaders(ColorizeProbeGlyphs(m.tableView()), m.sectionLabels()) + "\n")

	if m.stoppingName != "" {
		sb.WriteString(m.viewLine("  Stopping "+m.stoppingName+"… (waiting for guest shutdown)") + "\n")
	}
	if m.flashMessage != "" {
		style := lipgloss.NewStyle().Foreground(ColorPrimary)
		if m.flashIsError {
			style = lipgloss.NewStyle().Foreground(lipgloss.Color("#ef4444"))
		}
		sb.WriteString(m.viewLine(style.Render("  "+m.flashMessage)) + "\n")
	}

	if m.deviceLegend {
		sb.WriteString(m.viewLine(pickerHint.Render("  "+DeviceTableLegend(visible))) + "\n")
	}

	if idx := m.itemIndexForRow(m.table.Cursor()); idx >= 0 && idx < len(visible) && visible[idx].Insecure {
		sb.WriteString(m.viewLine(pickerInsecure.Render("  "+GlyphInsecure+"  Connection is not secured with mTLS. PKI support is coming soon.")) + "\n")
	}

	if m.scanning {
		sb.WriteString("\n" + m.viewLine(pickerScanning.Render("  Scanning for more results...")) + "\n")
	}

	if hint := m.selectedHint(); hint != "" {
		if !m.scanning {
			sb.WriteString("\n")
		}
		sb.WriteString(m.viewLine(pickerHint.Render("  "+hint)) + "\n")
	}

	return sb.String()
}

func (m PickerModel) selectedHint() string {
	visible := m.visibleItems()
	idx := m.itemIndexForRow(m.table.Cursor())
	if idx < 0 || idx >= len(visible) {
		return ""
	}
	return strings.TrimSpace(visible[idx].Hint)
}

func (m PickerModel) viewLine(line string) string {
	if m.width <= 0 {
		return line
	}
	return CropANSIView(line, 0, m.width)
}

func (m PickerModel) tableView() string {
	if !m.Filterable || m.filter == "" {
		return m.table.View()
	}
	// Highlight filter matches in the Name column. The cell layout is
	// 1 padding + content + 1 padding per column; the Name column is first
	// unless the ✦ default column (width 3 + padding) precedes it.
	cols := m.table.Columns()
	nameIdx := 0
	start := 1
	if m.OnSetDefault != nil && len(cols) > 1 {
		nameIdx = 1
		start += cols[0].Width + 2
	}
	if nameIdx >= len(cols) {
		return m.table.View()
	}
	view := HighlightMatches(m.table.FullView(), m.filter, start, start+cols[nameIdx].Width)
	if w := m.table.ViewportWidth(); w > 0 {
		view = CropANSIView(view, m.table.ScrollOffset(), w)
	}
	return view
}

// Cancelled returns true if the user quit the picker without selecting (e.g. Ctrl+C).
func (m PickerModel) Cancelled() bool {
	return m.quitting
}

func (m PickerModel) Selected() *PickerItem {
	return m.selected
}

// Glyphs marking a warning state on a device row, and the legend entries that
// explain them. Each glyph has exactly one meaning across every device table.
const (
	GlyphOutdated = "⚠"
	GlyphInsecure = "!"

	LegendOutdated = GlyphOutdated + " agent older than CLI"
	LegendInsecure = GlyphInsecure + " not using mTLS"
)

// DeviceTableLegendBase documents the glyphs the device table always explains.
// These are common enough that their absence from a row is itself informative,
// so they stay listed whether or not any row carries them.
const DeviceTableLegendBase = "● provisioned  ○ unprovisioned  ✦ default"

// DeviceTableLegend documents the base glyphs plus each warning glyph that
// items actually render, so a warning is never advertised when none is shown.
func DeviceTableLegend(items []PickerItem) string {
	legend := DeviceTableLegendBase
	for _, entry := range deviceLegendWarnings(items) {
		legend += "  " + entry
	}
	return legend
}

// deviceLegendWarnings returns the legend entries for the warning glyphs items
// render, in glyph order. Empty when no row carries one.
func deviceLegendWarnings(items []PickerItem) []string {
	var outdated, insecure bool
	for _, item := range items {
		outdated = outdated || itemShowsOutdated(item)
		insecure = insecure || item.Insecure
	}
	var entries []string
	if outdated {
		entries = append(entries, LegendOutdated)
	}
	if insecure {
		entries = append(entries, LegendInsecure)
	}
	return entries
}

// DeviceWarningLegend documents only the warning glyphs items render, for
// tables that have no provisioned/default markers to explain. Empty when
// nothing is marked, so the caller can omit the line entirely.
func DeviceWarningLegend(items []PickerItem) string {
	return strings.Join(deviceLegendWarnings(items), "  ")
}

// itemShowsOutdated reports whether the Agent cell renders the outdated glyph.
// A pending probe shows a spinner and a failed one may show no version at all,
// and neither can carry a marker. An offline row renders "offline" instead of
// a version, and its version is cached metadata no live probe has confirmed —
// update hints must never fire from that alone.
func itemShowsOutdated(item PickerItem) bool {
	return item.AgentOutdated && item.AgentVersion != "" &&
		item.Probe != ProbePending && item.Probe != ProbeOffline
}

type pickerColumnDef struct {
	title    string
	minWidth int
	value    func(PickerItem) string
	required bool
	optional bool // hidden when no item has a value, even with fixed columns
}

// provisionedGlyph maps the PickerItem.Provisioned state to the 1-char glyph
// rendered in the leading marker column. See DeviceTableLegend.
func provisionedGlyph(provisioned string) string {
	switch provisioned {
	case "Provisioned":
		return "●"
	case "Unprovisioned":
		return "○"
	}
	return ""
}

func pickerColumnDefsFromColumns(columns []PickerColumn) []pickerColumnDef {
	if len(columns) == 0 {
		return nil
	}
	defs := make([]pickerColumnDef, 0, len(columns))
	for _, col := range columns {
		value := col.Value
		if value == nil {
			value = func(PickerItem) string { return "" }
		}
		defs = append(defs, pickerColumnDef{
			title:    col.Title,
			minWidth: col.MinWidth,
			value:    value,
			required: col.Required,
		})
	}
	return defs
}

var pickerColumnDefs = []pickerColumnDef{
	{
		title:    "Name",
		minWidth: 18,
		value: func(item PickerItem) string {
			return item.Name
		},
		required: true,
	},
	{
		title:    "Type",
		minWidth: 12,
		value: func(item PickerItem) string {
			if item.USB != "" && !strings.Contains(item.Type, "USB") {
				return "USB, " + item.Type
			}
			return item.Type
		},
	},
	{
		title:    "Address",
		minWidth: 14,
		value: func(item PickerItem) string {
			return displayAddress(item.Address)
		},
	},
	{
		title:    "Description",
		minWidth: 20,
		value: func(item PickerItem) string {
			return item.Description
		},
	},
}

var pickerDeviceColumnDefs = []pickerColumnDef{
	pickerColumnDefs[0],
	pickerColumnDefs[1],
	pickerColumnDefs[2],
	{
		title:    "Agent",
		minWidth: 7,
		value: func(item PickerItem) string {
			val := probeColumnValue(item.Probe, item.AgentVersion, item.ProbeFrame)
			if itemShowsOutdated(item) {
				val += " " + GlyphOutdated
			}
			return val
		},
	},
	{
		title:    "OS",
		minWidth: 4,
		value: func(item PickerItem) string {
			return probeColumnValue(item.Probe, formatOSNameVersion(item.OS, item.OSVersion), item.ProbeFrame)
		},
	},
	{
		title:    "Description",
		minWidth: 20,
		value: func(item PickerItem) string {
			return item.Description
		},
		optional: true,
	},
}

func newPickerTable() BubbleTable {
	return NewBubbleTable(true, nil)
}

// visibleItems returns the items that pass the active filter, in display
// order. With no filter (or filtering disabled) it returns the full list, so
// table cursor indexes always address this slice.
func (m PickerModel) visibleItems() []PickerItem {
	if !m.Filterable || m.filter == "" {
		return m.items
	}
	query := strings.ToLower(m.filter)
	out := make([]PickerItem, 0, len(m.items))
	for _, item := range m.items {
		if strings.Contains(strings.ToLower(item.Name), query) {
			out = append(out, item)
		}
	}
	return out
}

func (m *PickerModel) currentCursorKey() string {
	visible := m.visibleItems()
	if idx := m.itemIndexForRow(m.table.Cursor()); idx >= 0 && idx < len(visible) {
		return strings.ToLower(pickerItemKey(visible[idx]))
	}
	return ""
}

// itemIndexForRow maps a table row index to its index in the visible-items
// slice, or returns -1 when the row is a section header or out of range.
func (m PickerModel) itemIndexForRow(row int) int {
	if row < 0 || row >= len(m.rowItem) {
		return -1
	}
	return m.rowItem[row]
}

// rowForItemIndex maps a visible-items index back to its table row, or -1 if
// the item has no row (should not happen for in-range indices).
func (m PickerModel) rowForItemIndex(itemIdx int) int {
	for row, idx := range m.rowItem {
		if idx == itemIdx {
			return row
		}
	}
	return -1
}

// snapCursorToSelectable nudges the cursor off a section-header row onto the
// nearest selectable row, searching first in dir (+1 down, -1 up) and then the
// other way when the list edge is reached. It is a no-op when the cursor is
// already on a selectable row, which is always the case for headerless pickers.
func (m *PickerModel) snapCursorToSelectable(dir int) {
	n := len(m.rowItem)
	if n == 0 {
		return
	}
	cursor := m.table.Cursor()
	if cursor >= 0 && cursor < n && m.rowItem[cursor] >= 0 {
		return
	}
	for i := cursor; i >= 0 && i < n; i += dir {
		if m.rowItem[i] >= 0 {
			m.table.SetCursor(i)
			return
		}
	}
	for i := cursor; i >= 0 && i < n; i -= dir {
		if m.rowItem[i] >= 0 {
			m.table.SetCursor(i)
			return
		}
	}
}

func (m *PickerModel) refreshTable() {
	m.refreshTableWithCursorKey(m.currentCursorKey())
}

func (m *PickerModel) refreshTableWithCursorKey(cursorKey string) {
	// Stamp the current spinner frame onto pending rows so the Agent/OS columns
	// animate as the table is rebuilt.
	frame := m.spinner.View()
	for i := range m.items {
		if m.items[i].Probe == ProbePending {
			m.items[i].ProbeFrame = frame
		}
	}

	// Sort items for a stable, predictable display order. When SortKey is set,
	// it takes precedence; otherwise sort by name (using DedupKey if present).
	sort.SliceStable(m.items, func(i, j int) bool {
		ki := m.items[i].SortKey
		if ki == "" {
			ki = strings.ToLower(pickerItemKey(m.items[i]))
		}
		kj := m.items[j].SortKey
		if kj == "" {
			kj = strings.ToLower(pickerItemKey(m.items[j]))
		}
		return ki < kj
	})

	m.rebuildSeenIdx()

	visible := m.visibleItems()
	hasDefaultCol := m.OnSetDefault != nil
	cols, itemRows := pickerTableDataForColumns(visible, m.DefaultKey, hasDefaultCol, m.columns, m.fixedColumns)
	// Determine which column index carries the section label. When a marker
	// column (default indicator) is present it occupies cols[0] with an empty
	// title; the first data column is then at index 1.
	labelOffset := 0
	if len(cols) > 0 && cols[0].Title == "" {
		labelOffset = 1
	}
	rows, rowItem := withSectionHeaders(visible, itemRows, len(cols), labelOffset)
	m.rowItem = rowItem
	m.table.SetRows(nil)
	m.table.SetColumns(cols)
	m.table.SetRows(rows)

	// Restore cursor to the same item when possible. If that item disappeared,
	// keep the cursor near its previous position and clamp it into range.
	if len(rows) > 0 {
		visIdx := -1
		if cursorKey != "" {
			for i, item := range visible {
				if strings.ToLower(pickerItemKey(item)) == cursorKey {
					visIdx = i
					break
				}
			}
		}
		if visIdx >= 0 {
			m.table.SetCursor(m.rowForItemIndex(visIdx))
		} else if m.table.Cursor() < 0 {
			m.table.SetCursor(0)
		} else if m.table.Cursor() >= len(rows) {
			m.table.SetCursor(len(rows) - 1)
		}
		// Keep the cursor off a leading/inherited section header.
		m.snapCursorToSelectable(1)
	}

	m.table.SetWidth(PickerTableWidth(m.table.Columns()))
	m.table.SetHeight(PickerTableHeight(len(rows), m.height))
}

func pickerItemKey(item PickerItem) string {
	if item.DedupKey != "" {
		return item.DedupKey
	}
	return item.Name
}

// rebuildSeenIdx re-derives the key index from the current item positions.
// Keys are always stored lowercase to match the lookup in Update.
func (m *PickerModel) rebuildSeenIdx() {
	for k := range m.seenIdx {
		delete(m.seenIdx, k)
	}
	for i, item := range m.items {
		m.seenIdx[strings.ToLower(pickerItemKey(item))] = i
	}
}

// removeItemByKey drops the row with the given lowercase dedup key, reporting
// whether one was found.
func (m *PickerModel) removeItemByKey(key string) bool {
	for i, item := range m.items {
		if strings.ToLower(pickerItemKey(item)) == key {
			m.items = append(m.items[:i], m.items[i+1:]...)
			m.rebuildSeenIdx()
			return true
		}
	}
	return false
}

// withSectionHeaders interleaves non-selectable section-header rows ahead of
// the first item of each section. It returns the full row set (headers + item
// rows) and a parallel rowItem slice mapping each row to its visible-items
// index (-1 for headers). When no visible item sets a Section, the item rows
// are returned unchanged with an identity mapping, preserving the headerless
// picker's exact behavior.
func withSectionHeaders(visible []PickerItem, itemRows []bubbleTable.Row, ncols, labelOffset int) ([]bubbleTable.Row, []int) {
	hasSection := false
	for _, item := range visible {
		if item.Section != "" {
			hasSection = true
			break
		}
	}
	if !hasSection {
		rowItem := make([]int, len(itemRows))
		for i := range rowItem {
			rowItem[i] = i
		}
		return itemRows, rowItem
	}

	rows := make([]bubbleTable.Row, 0, len(itemRows)+2)
	rowItem := make([]int, 0, len(itemRows)+2)
	currentSection := ""
	for i, item := range visible {
		if i >= len(itemRows) {
			break
		}
		if item.Section != "" && item.Section != currentSection {
			currentSection = item.Section
			rows = append(rows, sectionHeaderRow(currentSection, ncols, labelOffset))
			rowItem = append(rowItem, -1)
		}
		rows = append(rows, itemRows[i])
		rowItem = append(rowItem, i)
	}
	return rows, rowItem
}

// sectionHeaderPrefix marks a section-header cell so it reads as a group title
// rather than a selectable row. It is a plain-text rule (see sectionHeaderRow
// for why styling can't live in the cell); colorizeSectionHeaders keys on it to
// apply color after layout.
const sectionHeaderPrefix = "── "

// sectionHeaderRow builds a non-selectable header row whose first column shows
// the section label prefixed with a rule (── WendyOS) and whose remaining
// columns are blank.
//
// The label is intentionally plain text, not a lipgloss-styled string: the
// underlying bubbles table truncates every cell with runewidth.Truncate, which
// is not ANSI-aware. A styled value's escape bytes count toward the column
// width, so truncation cuts inside the escape sequence and the terminal renders
// garbage. The rule prefix keeps the header distinguishable even on terminals
// with no color; colorizeSectionHeaders adds color after layout for the rest.
func sectionHeaderRow(label string, ncols, labelOffset int) bubbleTable.Row {
	row := make(bubbleTable.Row, max(ncols, 1))
	cell := sectionHeaderPrefix + label
	if labelOffset < len(row) {
		row[labelOffset] = cell
	} else {
		row[0] = cell
	}
	return row
}

// pickerSectionHeader styles section-header lines: a bold accent so a group
// title (── WendyOS) stands apart from the selectable device rows below it.
var pickerSectionHeader = lipgloss.NewStyle().Bold(true).Foreground(ColorPrimary)

// sectionLabels returns the distinct section names present in the picker, in the
// order they first appear in the item list. Empty when no item sets a Section,
// so a headerless picker skips colorizeSectionHeaders entirely.
func (m PickerModel) sectionLabels() []string {
	var labels []string
	seen := make(map[string]bool)
	for _, item := range m.items {
		if item.Section != "" && !seen[item.Section] {
			seen[item.Section] = true
			labels = append(labels, item.Section)
		}
	}
	return labels
}

// colorizeSectionHeaders applies pickerSectionHeader to the header lines of an
// already-rendered table view, matching lines by the plain-text rule prefix and
// a known section label. The section cell is kept plain inside the table (see
// sectionHeaderRow); color is applied here, after layout, so it never counts
// toward column widths or trips the table's non-ANSI-aware truncation.
func colorizeSectionHeaders(view string, labels []string) string {
	if len(labels) == 0 {
		return view
	}
	lines := strings.Split(view, "\n")
	for i, line := range lines {
		content := strings.TrimLeft(line, " ")
		indent := line[:len(line)-len(content)]
		for _, label := range labels {
			if strings.HasPrefix(content, sectionHeaderPrefix+label) {
				lines[i] = indent + pickerSectionHeader.Render(strings.TrimRight(content, " "))
				break
			}
		}
	}
	return strings.Join(lines, "\n")
}

func pickerActiveColumnsForDefs(items []PickerItem, defs []pickerColumnDef, fixed bool) []pickerColumnDef {
	if len(defs) == 0 {
		defs = pickerColumnDefs
	}
	var active []pickerColumnDef
	for _, def := range defs {
		if def.required || (fixed && !def.optional) {
			active = append(active, def)
			continue
		}
		for _, item := range items {
			if def.value(item) != "" {
				active = append(active, def)
				break
			}
		}
	}
	return active
}

// PickerTableData builds the shared picker table columns and rows for items.
func PickerTableData(items []PickerItem, defaultKey string, hasDefaultCol bool) ([]bubbleTable.Column, []bubbleTable.Row) {
	return pickerTableDataForColumns(items, defaultKey, hasDefaultCol, nil, false)
}

// PickerDeviceTableData builds the stable device table used by device pickers
// and discover output.
func PickerDeviceTableData(items []PickerItem, defaultKey string, hasDefaultCol bool) ([]bubbleTable.Column, []bubbleTable.Row) {
	return pickerTableDataForColumns(items, defaultKey, hasDefaultCol, pickerDeviceColumnDefs, true)
}

func pickerTableDataForColumns(items []PickerItem, defaultKey string, hasDefaultCol bool, defs []pickerColumnDef, fixed bool) ([]bubbleTable.Column, []bubbleTable.Row) {
	activeCols := pickerActiveColumnsForDefs(items, defs, fixed)
	hasMarkerCol := hasDefaultCol || anyProvisionedGlyph(items)
	rows := pickerRows(items, activeCols, defaultKey, hasDefaultCol, hasMarkerCol)
	return pickerColumns(rows, activeCols, hasMarkerCol), rows
}

func anyProvisionedGlyph(items []PickerItem) bool {
	for _, item := range items {
		if provisionedGlyph(item.Provisioned) != "" {
			return true
		}
	}
	return false
}

func pickerRows(items []PickerItem, cols []pickerColumnDef, defaultKey string, hasDefaultCol, hasMarkerCol bool) []bubbleTable.Row {
	rows := make([]bubbleTable.Row, 0, len(items))
	for _, item := range items {
		var row bubbleTable.Row
		// The marker column combines the provisioned-state glyph with the
		// ✦ default indicator (see DeviceTableLegend).
		if hasMarkerCol {
			cell := provisionedGlyph(item.Provisioned)
			if hasDefaultCol && pickerItemMatchesDefaultKey(item, defaultKey) {
				if cell != "" {
					cell += " "
				}
				cell += "✦"
			}
			row = append(row, cell)
		}
		for _, col := range cols {
			val := col.value(item)
			if col.required && item.Insecure {
				val += " " + GlyphInsecure
			}
			row = append(row, val)
		}
		rows = append(rows, row)
	}
	return rows
}

func pickerItemMatchesDefaultKey(item PickerItem, defaultKey string) bool {
	defaultKey = strings.ToLower(strings.TrimSpace(defaultKey))
	if defaultKey == "" {
		return false
	}
	for _, key := range item.DefaultKeys {
		if strings.ToLower(strings.TrimSpace(key)) == defaultKey {
			return true
		}
	}
	key := strings.ToLower(strings.TrimSpace(item.DedupKey))
	if key == "" {
		key = strings.ToLower(strings.TrimSpace(item.Name))
	}
	return key == defaultKey
}

func pickerColumns(rows []bubbleTable.Row, defs []pickerColumnDef, hasMarkerCol bool) []bubbleTable.Column {
	cols := make([]bubbleTable.Column, 0, len(defs)+1)
	offset := 0
	if hasMarkerCol {
		cols = append(cols, bubbleTable.Column{Title: "", Width: 3})
		offset = 1
	}
	for i, def := range defs {
		width := lipgloss.Width(def.title)
		for _, row := range rows {
			rowIdx := i + offset
			if rowIdx >= len(row) {
				continue
			}
			width = max(width, lipgloss.Width(row[rowIdx]))
		}
		width += 2
		width = max(width, def.minWidth)
		cols = append(cols, bubbleTable.Column{Title: def.title, Width: width})
	}
	return cols
}

func PickerTableWidth(cols []bubbleTable.Column) int {
	total := 0
	for _, col := range cols {
		total += col.Width + 2
	}
	return total
}

func PickerTableHeight(rowCount, windowHeight int) int {
	height := max(rowCount+1, 4)
	if windowHeight > 0 {
		return min(height, max(windowHeight-5, 4))
	}
	return min(height, 12)
}

func (m PickerModel) canScrollTable() bool {
	return m.table.CanScroll()
}

func (m PickerModel) tableViewportWidth() int {
	if width := m.table.ViewportWidth(); width > 0 {
		return width
	}
	return PickerTableWidth(m.table.Columns())
}
