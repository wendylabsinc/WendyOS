package chat

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	memorySchemaVersion = 1
	memoryMaxFileBytes  = 16 * 1024
	memoryMaxFiles      = 2000
)

// MemoryEntry is an explicitly recorded lesson, procedure, fact, or preference.
// Evidence describes the observed result supporting the memory, not a transcript.
type MemoryEntry struct {
	ID        string    `json:"id"`
	Scope     string    `json:"scope"`
	Workspace string    `json:"workspace,omitempty"`
	Device    string    `json:"device,omitempty"`
	Kind      string    `json:"kind"`
	Title     string    `json:"title"`
	Content   string    `json:"content"`
	Evidence  string    `json:"evidence"`
	UpdatedAt time.Time `json:"updated_at"`
}

type MemoryInput struct {
	Scope    string `json:"scope,omitempty"`
	Device   string `json:"device,omitempty"`
	Kind     string `json:"kind"`
	Title    string `json:"title"`
	Content  string `json:"content"`
	Evidence string `json:"evidence"`
}

type memoryDocument struct {
	SchemaVersion int `json:"schema_version"`
	MemoryEntry
}

// MemoryStore stores independent, atomically replaced files. Separate sessions
// can add unrelated memories without overwriting a shared index.
type MemoryStore struct {
	directory string
	workspace string
	device    string
}

// NewMemoryStore resolves the workspace but does not create or modify files.
func NewMemoryStore(directory, workspace, device string) (*MemoryStore, error) {
	if directory == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("locate memory directory: %w", err)
		}
		directory = filepath.Join(home, ".wendy", "memory")
	}
	directory, err := filepath.Abs(directory)
	if err != nil {
		return nil, fmt.Errorf("resolve memory directory: %w", err)
	}
	if workspace == "" {
		workspace = "."
	}
	workspace, err = filepath.Abs(workspace)
	if err == nil {
		workspace, err = filepath.EvalSymlinks(workspace)
	}
	if err != nil {
		return nil, fmt.Errorf("resolve memory workspace: %w", err)
	}
	info, err := os.Stat(workspace)
	if err != nil {
		return nil, fmt.Errorf("inspect memory workspace: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("memory workspace must be a directory")
	}
	device = strings.TrimSpace(device)
	if err := validateMemoryText("device", device, 256, false); err != nil {
		return nil, err
	}
	return &MemoryStore{directory: directory, workspace: workspace, device: device}, nil
}

func (s *MemoryStore) Directory() string { return s.directory }

func (s *MemoryStore) Save(ctx context.Context, input MemoryInput) (MemoryEntry, error) {
	if err := ctx.Err(); err != nil {
		return MemoryEntry{}, err
	}
	entry := MemoryEntry{
		Scope: strings.TrimSpace(input.Scope), Device: strings.TrimSpace(input.Device),
		Kind: strings.TrimSpace(input.Kind), Title: strings.Join(strings.Fields(input.Title), " "),
		Content: strings.TrimSpace(input.Content), Evidence: strings.TrimSpace(input.Evidence),
		UpdatedAt: time.Now().UTC(),
	}
	if entry.Scope == "" {
		entry.Scope = "workspace"
	}
	if entry.Scope == "workspace" {
		entry.Workspace = s.workspace
	}
	if err := validateMemoryEntry(entry); err != nil {
		return MemoryEntry{}, err
	}
	entry.ID = memoryID(entry)
	data, err := json.MarshalIndent(memoryDocument{SchemaVersion: memorySchemaVersion, MemoryEntry: entry}, "", "  ")
	if err != nil {
		return MemoryEntry{}, fmt.Errorf("encode memory: %w", err)
	}
	data = append(data, '\n')
	if len(data) > memoryMaxFileBytes {
		return MemoryEntry{}, errors.New("encoded memory exceeds 16 KiB")
	}
	root, err := s.openDirectory(true)
	if err != nil {
		return MemoryEntry{}, err
	}
	defer root.Close()
	name := entry.ID + ".json"
	_, err = readMemory(root, name)
	if err != nil && !os.IsNotExist(err) {
		return MemoryEntry{}, fmt.Errorf("cannot replace invalid memory %s: %w", entry.ID, err)
	}
	if os.IsNotExist(err) {
		entries, err := listMemoryFiles(root)
		if err != nil {
			return MemoryEntry{}, err
		}
		if len(entries) >= memoryMaxFiles {
			return MemoryEntry{}, errors.New("memory directory is full; delete an old memory before adding another")
		}
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return MemoryEntry{}, fmt.Errorf("name temporary memory: %w", err)
	}
	temporary := ".memory-" + hex.EncodeToString(random[:]) + ".tmp"
	f, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return MemoryEntry{}, fmt.Errorf("create memory: %w", err)
	}
	defer root.Remove(temporary)
	defer f.Close()
	if err := f.Chmod(0600); err != nil {
		return MemoryEntry{}, fmt.Errorf("protect memory: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		return MemoryEntry{}, fmt.Errorf("write memory: %w", err)
	}
	if err := f.Sync(); err != nil {
		return MemoryEntry{}, fmt.Errorf("sync memory: %w", err)
	}
	if err := f.Close(); err != nil {
		return MemoryEntry{}, fmt.Errorf("close memory: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return MemoryEntry{}, err
	}
	if err := root.Rename(temporary, name); err != nil {
		return MemoryEntry{}, fmt.Errorf("replace memory: %w", err)
	}
	return entry, nil
}

// Search returns only global, current-workspace, and relevant-device memories.
// Invalid documents and unsafe file types are ignored; they never become model
// context. An explicit device or a complete device name in the query can recall
// device knowledge even when another device is preferred for this session.
func (s *MemoryStore) Search(ctx context.Context, query, device string, limit int) ([]MemoryEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	device = strings.TrimSpace(device)
	if err := validateMemoryText("device", device, 256, false); err != nil {
		return nil, err
	}
	if device == "" {
		device = s.device
	}
	if limit <= 0 {
		limit = 8
	}
	if limit > 50 {
		limit = 50
	}
	// Only a bounded portion of a long user message participates in retrieval.
	if len(query) > 8192 {
		query = query[:8192]
	}
	query = strings.ToLower(query)
	tokens := memoryTokens(query)
	if len(tokens) > 128 {
		tokens = tokens[:128]
	}
	root, err := s.openDirectory(false)
	if errors.Is(err, os.ErrNotExist) {
		return []MemoryEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	files, err := listMemoryFiles(root)
	if err != nil {
		return nil, err
	}
	type match struct {
		entry MemoryEntry
		score int
	}
	matches := make([]match, 0)
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !isMemoryFilename(file.Name()) || !file.Type().IsRegular() {
			continue
		}
		entry, err := readMemory(root, file.Name())
		if err != nil {
			continue
		}
		switch entry.Scope {
		case "workspace":
			if entry.Workspace != s.workspace {
				continue
			}
		case "device":
			if !strings.EqualFold(entry.Device, device) && !memoryMentionsDevice(query, entry.Device) {
				continue
			}
		}
		score := memoryRelevance(entry, tokens)
		if len(tokens) > 0 && score == 0 {
			continue
		}
		matches = append(matches, match{entry: entry, score: score})
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		if !matches[i].entry.UpdatedAt.Equal(matches[j].entry.UpdatedAt) {
			return matches[i].entry.UpdatedAt.After(matches[j].entry.UpdatedAt)
		}
		return matches[i].entry.ID < matches[j].entry.ID
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}
	entries := make([]MemoryEntry, len(matches))
	for i := range matches {
		entries[i] = matches[i].entry
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

// Delete requires an exact memory ID. It can remove memories for other devices
// or workspaces so users retain control after switching their session context.
func (s *MemoryStore) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validMemoryID(id) {
		return errors.New("memory ID must be 64 lowercase hexadecimal characters")
	}
	root, err := s.openDirectory(false)
	if err != nil {
		return err
	}
	defer root.Close()
	name := id + ".json"
	if _, err := readMemory(root, name); err != nil {
		return fmt.Errorf("read memory before deleting: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := root.Remove(name); err != nil {
		return fmt.Errorf("delete memory: %w", err)
	}
	return nil
}

func (s *MemoryStore) openDirectory(create bool) (*os.Root, error) {
	info, err := os.Lstat(s.directory)
	if os.IsNotExist(err) && create {
		if err := os.MkdirAll(s.directory, 0700); err != nil {
			return nil, fmt.Errorf("create memory directory: %w", err)
		}
		info, err = os.Lstat(s.directory)
	}
	if err != nil {
		return nil, fmt.Errorf("inspect memory directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("memory directory must be a directory, not a symbolic link")
	}
	root, err := os.OpenRoot(s.directory)
	if err != nil {
		return nil, fmt.Errorf("open memory directory: %w", err)
	}
	f, err := root.Open(".")
	if err != nil {
		root.Close()
		return nil, fmt.Errorf("open memory directory handle: %w", err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		root.Close()
		return nil, errors.New("memory directory changed while opening")
	}
	if create {
		if err := f.Chmod(0700); err != nil {
			root.Close()
			return nil, fmt.Errorf("protect memory directory: %w", err)
		}
	}
	return root, nil
}

func listMemoryFiles(root *os.Root) ([]os.DirEntry, error) {
	f, err := root.Open(".")
	if err != nil {
		return nil, fmt.Errorf("open memory listing: %w", err)
	}
	defer f.Close()
	files, err := f.ReadDir(memoryMaxFiles + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("list memories: %w", err)
	}
	if len(files) > memoryMaxFiles {
		return nil, errors.New("memory directory exceeds the 2000-file scan limit")
	}
	return files, nil
}

func readMemory(root *os.Root, name string) (MemoryEntry, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return MemoryEntry{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > memoryMaxFileBytes {
		return MemoryEntry{}, errors.New("memory must be a regular file no larger than 16 KiB")
	}
	f, err := root.Open(name)
	if err != nil {
		return MemoryEntry{}, err
	}
	defer f.Close()
	opened, err := f.Stat()
	// Another session may atomically replace a regular file between Lstat and
	// Open. Accept its complete replacement; validate its identity below.
	if err != nil || !opened.Mode().IsRegular() {
		return MemoryEntry{}, errors.New("opened memory is not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, memoryMaxFileBytes+1))
	if err != nil {
		return MemoryEntry{}, err
	}
	if len(data) > memoryMaxFileBytes {
		return MemoryEntry{}, errors.New("memory exceeds 16 KiB")
	}
	var document memoryDocument
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return MemoryEntry{}, errors.New("memory contains invalid JSON or unknown fields")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return MemoryEntry{}, errors.New("memory contains trailing data")
	}
	if document.SchemaVersion != memorySchemaVersion {
		return MemoryEntry{}, errors.New("unsupported memory schema version")
	}
	entry := document.MemoryEntry
	if err := validateMemoryEntry(entry); err != nil {
		return MemoryEntry{}, err
	}
	if !validMemoryID(entry.ID) || entry.ID != memoryID(entry) || name != entry.ID+".json" {
		return MemoryEntry{}, errors.New("memory ID does not match its scope and title")
	}
	return entry, nil
}

func validateMemoryEntry(entry MemoryEntry) error {
	switch entry.Scope {
	case "workspace":
		if !filepath.IsAbs(entry.Workspace) || filepath.Clean(entry.Workspace) != entry.Workspace || entry.Device != "" {
			return errors.New("workspace memory requires an absolute workspace and no device")
		}
	case "device":
		if strings.TrimSpace(entry.Device) == "" || entry.Workspace != "" {
			return errors.New("device memory requires an explicitly named device and no workspace")
		}
	case "global":
		if entry.Workspace != "" || entry.Device != "" {
			return errors.New("global memory cannot specify a workspace or device")
		}
	default:
		return errors.New("memory scope must be workspace, device, or global")
	}
	switch entry.Kind {
	case "procedure", "lesson", "fact", "preference":
	default:
		return errors.New("memory kind must be procedure, lesson, fact, or preference")
	}
	for _, field := range []struct {
		name     string
		value    string
		max      int
		required bool
	}{
		{"title", entry.Title, 160, true}, {"content", entry.Content, 6000, true},
		{"evidence", entry.Evidence, 2000, true}, {"device", entry.Device, 256, false},
		{"workspace", entry.Workspace, 4096, false},
	} {
		if err := validateMemoryText(field.name, field.value, field.max, field.required); err != nil {
			return err
		}
	}
	if entry.UpdatedAt.IsZero() {
		return errors.New("memory requires an update timestamp")
	}
	return nil
}

func validateMemoryText(name, value string, max int, required bool) error {
	if required && strings.TrimSpace(value) == "" {
		return fmt.Errorf("memory %s is required", name)
	}
	if len(value) > max {
		return fmt.Errorf("memory %s exceeds %d bytes", name, max)
	}
	if !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("memory %s must be valid UTF-8 without NUL bytes", name)
	}
	return nil
}

func memoryID(entry MemoryEntry) string {
	identity, _ := json.Marshal([]string{entry.Scope, entry.Workspace, strings.ToLower(strings.TrimSpace(entry.Device)), strings.ToLower(strings.Join(strings.Fields(entry.Title), " "))})
	sum := sha256.Sum256(identity)
	return hex.EncodeToString(sum[:])
}

func validMemoryID(id string) bool {
	if len(id) != sha256.Size*2 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func isMemoryFilename(name string) bool {
	return strings.HasSuffix(name, ".json") && validMemoryID(strings.TrimSuffix(name, ".json"))
}

func memoryTokens(text string) []string {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
	seen := make(map[string]bool)
	tokens := make([]string, 0, len(fields))
	for _, token := range fields {
		if !seen[token] {
			seen[token] = true
			tokens = append(tokens, token)
		}
	}
	return tokens
}

func memoryRelevance(entry MemoryEntry, query []string) int {
	weights := make(map[string]int)
	for _, field := range []struct {
		text   string
		weight int
	}{{entry.Title, 6}, {entry.Content, 3}, {entry.Evidence, 1}, {entry.Device, 2}} {
		for _, token := range memoryTokens(field.text) {
			weights[token] += field.weight
		}
	}
	score := 0
	for _, token := range query {
		score += weights[token]
	}
	return score
}

func memoryMentionsDevice(query, device string) bool {
	device = strings.ToLower(strings.TrimSpace(device))
	if device == "" {
		return false
	}
	word := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' || r == '-' }
	for offset := 0; offset <= len(query)-len(device); {
		relative := strings.Index(query[offset:], device)
		if relative < 0 {
			return false
		}
		start := offset + relative
		end := start + len(device)
		before, _ := utf8.DecodeLastRuneInString(query[:start])
		after, _ := utf8.DecodeRuneInString(query[end:])
		if (start == 0 || !word(before)) && (end == len(query) || !word(after)) {
			return true
		}
		offset = start + len(device)
	}
	return false
}
