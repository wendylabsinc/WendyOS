package chat

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Keep presentation separate from memoryJSON, which supplies complete records
// to the model. Store the notes in the transcript so resizing can reflow them.
type memoryDisplay struct {
	entries []MemoryEntry
	query   string
	enabled bool
	detail  bool
}

type memoryDisplayLine struct {
	text  string
	style lipgloss.Style
}

func (m *chatModel) showMemoryNotes(query string) {
	entries, err := m.opts.Engine.MemoryNotes(m.ctx, query)
	if err != nil {
		m.appendEntry("error", "Memory unavailable", err.Error())
		return
	}
	m.appendMemoryEntry(chatEntry{
		kind: "memory", title: "Remembered notes",
		memory: &memoryDisplay{entries: entries, query: query, enabled: m.opts.Engine.MemoryEnabled()},
	})
}

func (m *chatModel) showMemoryNote(ref string) {
	entry, err := m.opts.Engine.MemoryNote(m.ctx, ref)
	if err != nil {
		m.appendEntry("error", "Could not open note", err.Error())
		return
	}
	m.appendMemoryEntry(chatEntry{
		kind: "memory", title: entry.Title,
		memory: &memoryDisplay{entries: []MemoryEntry{entry}, detail: true},
	})
}

func (m *chatModel) appendMemoryEntry(entry chatEntry) {
	// Open a long list or note at its beginning instead of its last paragraph.
	offset := 0
	if len(m.transcript) > 0 {
		content, _ := m.transcriptContent(m.viewport.Width)
		offset = strings.Count(content, "\n") + 2
	}
	m.transcript = append(m.transcript, entry)
	m.refreshTranscript()
	m.viewport.SetYOffset(offset)
}

func (d *memoryDisplay) lines() []memoryDisplayLine {
	var lines []memoryDisplayLine
	plain := lipgloss.NewStyle()
	add := func(text string, style lipgloss.Style) {
		lines = append(lines, memoryDisplayLine{text: text, style: style})
	}
	if d.detail && len(d.entries) == 1 {
		entry := d.entries[0]
		add(memoryMetadata(entry), chatDim)
		add("", plain)
		add(entry.Content, plain)
		add("", plain)
		add("Evidence", chatTitle)
		add(entry.Evidence, plain)
		add("", plain)
		if entry.Scope == "workspace" {
			add("Project: "+entry.Workspace, chatDim)
		}
		add("/memory to return to the list · /forget "+shortMemoryID(entry.ID)+" to delete", chatDim)
		return lines
	}

	state := "Memory on"
	if !d.enabled {
		state = "Memory paused · existing notes are kept"
	}
	count := fmt.Sprintf("%d notes", len(d.entries))
	if len(d.entries) == 1 {
		count = "1 note"
	}
	if len(d.entries) == 50 {
		count = "Up to 50 recent notes"
		if d.query != "" {
			count = "Up to 50 matching notes"
		}
	}
	add(count+" · "+state, chatDim)
	if d.query != "" {
		add("Search: "+chatSingleLine(d.query), chatDim)
	}
	if len(d.entries) == 0 {
		add("", plain)
		if d.query != "" {
			add("No notes match this search. Try different words or /memory to see recent notes.", plain)
		} else {
			add("No notes saved for this workspace or device yet.", plain)
			if d.enabled {
				add("Wendy remembers useful procedures, corrections, and preferences as you work.", chatDim)
			}
		}
		return lines
	}
	add("/memory <id> to read a note · /forget <id> to delete", chatDim)
	add("/memory search <words> to find notes", chatDim)
	for _, entry := range d.entries {
		add("", plain)
		add("• "+entry.Title, chatTitle)
		add(memoryMetadata(entry), chatDim)
		add(ansi.Truncate(chatSingleLine(entry.Content), 160, "…"), plain)
	}
	return lines
}

func memoryMetadata(entry MemoryEntry) string {
	kind := map[string]string{
		"procedure": "Procedure", "lesson": "Lesson", "fact": "Fact", "preference": "Preference",
	}[entry.Kind]
	scope := "All projects"
	switch entry.Scope {
	case "workspace":
		scope = "Project"
	case "device":
		scope = entry.Device
	}
	return strings.Join([]string{kind, scope, entry.UpdatedAt.Local().Format("2 Jan 2006"), shortMemoryID(entry.ID)}, " · ")
}

func shortMemoryID(id string) string {
	return id[:min(8, len(id))]
}
