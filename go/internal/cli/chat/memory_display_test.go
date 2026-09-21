package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func memoryDisplayModel(t *testing.T, inputs ...MemoryInput) (*chatModel, []MemoryEntry) {
	t.Helper()
	store, err := NewMemoryStore(t.TempDir(), t.TempDir(), "Woof")
	if err != nil {
		t.Fatal(err)
	}
	var entries []MemoryEntry
	for _, input := range inputs {
		entry, err := store.Save(context.Background(), input)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
	}
	m := uiModel(t, nil, NewMemoryTools(&uiExecutor{}, store), false)
	m.resize(100, 28)
	return m, entries
}

func enterMemoryCommand(t *testing.T, m *chatModel, prompt string) {
	t.Helper()
	m.composer.SetValue(prompt)
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.active || m.events != nil || m.turnID != 0 || len(m.queuedPrompts) != 0 {
		t.Fatalf("%q started or queued a model request", prompt)
	}
}

func TestUIMemoryPreviewKeepsDetailsAvailableWithoutRawRecord(t *testing.T) {
	content := "First check the front camera.\n" + strings.Repeat("Verify the current camera ID before reuse. ", 8) + "final-detail-marker"
	input := MemoryInput{
		Scope: "device", Device: "Woof", Kind: "procedure", Title: "Inspect the front camera",
		Content: content, Evidence: `camera_snapshot({"device_id":129}) returned a JPEG.`,
	}
	m, entries := memoryDisplayModel(t, input)
	entry := entries[0]
	enterMemoryCommand(t, m, "/memory")
	preview, _ := m.transcriptContent(1000)
	preview = ansi.Strip(preview)
	for _, want := range []string{entry.Title, "Procedure", "Woof", entry.ID[:8], "First check the front camera.", "…", "/memory <id>"} {
		if !strings.Contains(preview, want) {
			t.Errorf("preview is missing %q: %s", want, preview)
		}
	}
	for _, hidden := range []string{entry.ID, entry.Evidence, "final-detail-marker", `"updated_at":`, `"content":`, `"scope":`} {
		if strings.Contains(preview, hidden) {
			t.Errorf("preview exposed detailed record text %q", hidden)
		}
	}
	enterMemoryCommand(t, m, "/memory "+entry.ID[:8])
	detail, _ := m.transcriptContent(1000)
	detail = ansi.Strip(detail)
	if !strings.Contains(detail, entry.Content) || !strings.Contains(detail, "Evidence\n"+entry.Evidence) {
		t.Fatalf("opening the short ID lost the complete note or its evidence: %s", detail)
	}
	var modelEntries []MemoryEntry
	if err := json.Unmarshal([]byte(memoryJSON(entries)), &modelEntries); err != nil {
		t.Fatal(err)
	}
	if len(modelEntries) != 1 || modelEntries[0].ID != entry.ID || modelEntries[0].Content != entry.Content || modelEntries[0].Evidence != entry.Evidence {
		t.Fatal("compact display changed the full records available to the model")
	}
}

func TestUIMemorySearchRemainsLocalWhilePaused(t *testing.T) {
	m, entries := memoryDisplayModel(t,
		MemoryInput{Scope: "device", Device: "Woof", Kind: "fact", Title: "Camera connection", Content: "The front camera uses the video service.", Evidence: "The user supplied the service name."},
		MemoryInput{Scope: "workspace", Kind: "preference", Title: "Container preference", Content: "Use the application container.", Evidence: "The user requested this."},
	)
	enterMemoryCommand(t, m, "/memory off")
	enterMemoryCommand(t, m, "/memory search camera")
	text, _ := m.transcriptContent(200)
	text = ansi.Strip(text)
	for _, want := range []string{"Memory paused", "Search: camera", "1 note", entries[0].Title} {
		if !strings.Contains(text, want) {
			t.Errorf("paused search is missing %q: %s", want, text)
		}
	}
	if strings.Contains(text, entries[1].Title) || m.opts.Engine.MemoryEnabled() {
		t.Fatal("search must filter the notes without resuming memory")
	}
	enterMemoryCommand(t, m, "/memory "+entries[0].ID[:8])
	text, _ = m.transcriptContent(200)
	if !strings.Contains(ansi.Strip(text), entries[0].Evidence) || m.opts.Engine.MemoryEnabled() {
		t.Fatal("paused memory must still allow reading a note's evidence")
	}
	enterMemoryCommand(t, m, "/memory search nonexistentword")
	if view := ansi.Strip(m.viewport.View()); !strings.Contains(view, "No notes match this search") {
		t.Fatalf("empty search did not explain how to recover: %s", view)
	}
}

func TestUIMemoryAmbiguousForgetKeepsBothNotes(t *testing.T) {
	m, entries := memoryDisplayModel(t,
		MemoryInput{Scope: "global", Kind: "fact", Title: "Lookup collision 15669", Content: "First remembered note.", Evidence: "The user supplied this note."},
		MemoryInput{Scope: "global", Kind: "fact", Title: "Lookup collision 135542", Content: "Second remembered note.", Evidence: "The user supplied this note."},
	)
	if entries[0].ID[:8] != entries[1].ID[:8] {
		t.Fatal("test fixtures must share the displayed short ID")
	}
	enterMemoryCommand(t, m, "/forget "+entries[0].ID[:8])
	text, _ := m.transcriptContent(200)
	text = ansi.Strip(text)
	if !strings.Contains(text, "Could not forget note") || !strings.Contains(text, "ambiguous") {
		t.Fatalf("ambiguous deletion did not explain why the note could not be removed: %s", text)
	}
	for _, entry := range entries {
		if !strings.Contains(text, entry.Title) || !strings.Contains(text, entry.ID) {
			t.Errorf("ambiguous deletion did not identify candidate %q and its full ID", entry.Title)
		}
		if retained, err := m.opts.Engine.MemoryNote(context.Background(), entry.ID); err != nil || retained.ID != entry.ID {
			t.Errorf("ambiguous deletion removed %q: %v", entry.Title, err)
		}
	}
}

func TestUIMemoryLongListAndNoteOpenAtBeginningAcrossResize(t *testing.T) {
	var body strings.Builder
	for i := 0; i < 140; i++ {
		fmt.Fprintf(&body, "Line %03d: inspect 界 camera.\n", i)
	}
	input := MemoryInput{Scope: "workspace", Kind: "fact", Title: "Camera", Content: body.String(), Evidence: "The user supplied these inspection notes."}
	for _, mode := range []string{"list", "detail"} {
		t.Run(mode, func(t *testing.T) {
			inputs := []MemoryInput{input}
			if mode == "list" {
				for i := 1; i < 25; i++ {
					inputs = append(inputs, MemoryInput{Scope: "workspace", Kind: "fact", Title: fmt.Sprintf("Camera %02d", i), Content: "Inspect 界 camera for a clear image.", Evidence: "The user supplied this note."})
				}
			}
			m, entries := memoryDisplayModel(t, inputs...)
			m.appendEntry("assistant", "Earlier conversation", strings.Repeat("An earlier message.\n", 40))
			command, title := "/memory", "Remembered notes"
			if mode == "detail" {
				command, title = "/memory "+entries[0].ID[:8], "Camera"
			}
			enterMemoryCommand(t, m, command)
			for _, size := range [][2]int{{100, 28}, {35, 18}, {120, 24}, {12, 12}, {80, 28}} {
				m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
				view := ansi.Strip(m.viewport.View())
				if !strings.HasPrefix(view, title[:6]) || m.viewport.AtBottom() {
					t.Fatalf("size %v opened the %s away from its beginning: %q", size, mode, view)
				}
				if mode == "detail" && strings.Contains(view, "Line 139") {
					t.Fatalf("size %v jumped to the end of the note", size)
				}
				for _, line := range strings.Split(m.View(), "\n") {
					if !utf8.ValidString(line) || ansi.StringWidth(line) > size[0] {
						t.Fatalf("size %v produced an invalid or oversized line: %q", size, line)
					}
				}
				if height := strings.Count(m.View(), "\n") + 1; height > size[1] {
					t.Fatalf("size %v produced %d rows", size, height)
				}
			}
		})
	}
}

func TestUIMemoryRenderingSanitizesNoteFields(t *testing.T) {
	m := uiModel(t, nil, &uiExecutor{}, false)
	entry := MemoryEntry{
		ID: strings.Repeat("a", 64), Scope: "device", Device: "Woof\x1b[2J", Kind: "fact",
		Title: "Camera\x1b]52;c;Y2xpcGJvYXJk\a", Content: "A\u202e界\x1b[2J\nSecond line", Evidence: "Observed\x00 result",
	}
	for _, detail := range []bool{false, true} {
		m.transcript = []chatEntry{{kind: "memory", title: entry.Title, memory: &memoryDisplay{entries: []MemoryEntry{entry}, enabled: true, detail: detail}}}
		text, _ := m.transcriptContent(80)
		for _, forbidden := range []string{"\x1b[2J", "\x1b]52", "Y2xpcGJvYXJk", "\u202e", "\x00"} {
			if strings.Contains(text, forbidden) {
				t.Errorf("detail=%v: rendered unsafe note text %q", detail, forbidden)
			}
		}
		if !utf8.ValidString(text) || !strings.Contains(ansi.Strip(text), "A界") || !strings.Contains(ansi.Strip(text), "Second line") {
			t.Fatalf("detail=%v: sanitization lost readable Unicode text: %q", detail, text)
		}
	}
}
