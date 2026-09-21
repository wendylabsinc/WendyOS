package chat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Lookup resolves an exact ID or an unambiguous ID prefix across all contexts.
// References remain usable after switching workspaces or preferred devices.
func (s *MemoryStore) Lookup(ctx context.Context, ref string) (MemoryEntry, error) {
	if err := ctx.Err(); err != nil {
		return MemoryEntry{}, err
	}
	if len(ref) < 8 || len(ref) > 64 {
		return MemoryEntry{}, errors.New("memory ID must be 8–64 lowercase hexadecimal characters")
	}
	for _, c := range ref {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return MemoryEntry{}, errors.New("memory ID must be 8–64 lowercase hexadecimal characters")
		}
	}
	notFound := func() (MemoryEntry, error) {
		return MemoryEntry{}, fmt.Errorf("no remembered note matches %q", ref)
	}
	root, err := s.openDirectory(false)
	if errors.Is(err, os.ErrNotExist) {
		return notFound()
	}
	if err != nil {
		return MemoryEntry{}, err
	}
	defer root.Close()
	if len(ref) == 64 {
		entry, err := readMemory(root, ref+".json")
		if ctxErr := ctx.Err(); ctxErr != nil {
			return MemoryEntry{}, ctxErr
		}
		if errors.Is(err, os.ErrNotExist) {
			return notFound()
		}
		if err != nil {
			return MemoryEntry{}, fmt.Errorf("read remembered note: %w", err)
		}
		return entry, nil
	}
	files, err := listMemoryFiles(root)
	if err != nil {
		return MemoryEntry{}, err
	}
	var matches []MemoryEntry
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return MemoryEntry{}, err
		}
		if !isMemoryFilename(file.Name()) || !strings.HasPrefix(file.Name(), ref) || !file.Type().IsRegular() {
			continue
		}
		entry, err := readMemory(root, file.Name())
		if err != nil {
			continue
		}
		matches = append(matches, entry)
	}
	if err := ctx.Err(); err != nil {
		return MemoryEntry{}, err
	}
	if len(matches) == 0 {
		return notFound()
	}
	if len(matches) > 1 {
		choices := make([]string, len(matches))
		for i, entry := range matches {
			choices[i] = entry.ID + "  " + chatSingleLine(entry.Title)
		}
		return MemoryEntry{}, fmt.Errorf("memory ID %q is ambiguous. Use a full ID:\n%s", ref, strings.Join(choices, "\n"))
	}
	return matches[0], nil
}
