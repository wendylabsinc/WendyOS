package data

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
	"github.com/wendylabsinc/wendy/go/internal/shared/datadoc"
)

var (
	ErrInvalidDocumentName = errors.New("invalid document name")
	ErrNotStoredKind       = errors.New("only inference and watch documents are stored as documents; a collection is a campaign")
)

// StoredDocument is an inference or watch document as the agent keeps it, in
// data/documents/<kind>/<name>.json beside data/campaigns (spec §5.6).
type StoredDocument struct {
	Kind              string          `json:"kind"`
	Version           int             `json:"version"`
	Name              string          `json:"name"`
	Owner             string          `json:"owner,omitempty"`
	Revision          string          `json:"revision"`
	DeployedUnixNanos int64           `json:"deployed_unix_nanos"`
	Warnings          []string        `json:"warnings,omitempty"`
	Body              json.RawMessage `json:"body"`
}

// Ref is the document's "<kind>/<name>", the form owners are named in.
func (d StoredDocument) Ref() string { return d.Kind + "/" + d.Name }

// Inference decodes an inference document's body.
func (d StoredDocument) Inference() (InferenceDocument, error) {
	var document InferenceDocument
	if d.Kind != KindInference {
		return document, fmt.Errorf("%s is not an inference document", d.Ref())
	}
	err := json.Unmarshal(d.Body, &document)
	return document, err
}

// Watch decodes a watch document's body.
func (d StoredDocument) Watch() (Watch, error) {
	var watch Watch
	if d.Kind != KindWatch {
		return watch, fmt.Errorf("%s is not a watch document", d.Ref())
	}
	err := json.Unmarshal(d.Body, &watch)
	return watch, err
}

func storedKind(kind string) bool { return kind == KindInference || kind == KindWatch }

func (m *Manager) documentsDir(kind string) string {
	return filepath.Join(filepath.Dir(m.root), "documents", kind)
}

func (m *Manager) documentPath(kind, name string) (string, error) {
	if !storedKind(kind) {
		return "", ErrNotStoredKind
	}
	if !datadoc.ValidName(name) {
		return "", ErrInvalidDocumentName
	}
	return filepath.Join(m.documentsDir(kind), name+".json"), nil
}

// SaveDocument stores a parsed inference or watch document, replacing any
// earlier one of the same kind and name. Owner and lease rules are the
// caller's: this is storage only.
func (m *Manager) SaveDocument(doc datadoc.Document) (StoredDocument, error) {
	path, err := m.documentPath(doc.Header.Kind, doc.Header.Name)
	if err != nil {
		return StoredDocument{}, err
	}
	body, err := json.Marshal(doc.Body)
	if err != nil {
		return StoredDocument{}, err
	}
	stored := StoredDocument{Kind: doc.Header.Kind, Version: doc.Header.Version, Name: doc.Header.Name, Owner: doc.Header.Owner,
		Revision: doc.Revision, DeployedUnixNanos: time.Now().UnixNano(), Warnings: doc.Warnings, Body: body}
	encoded, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return StoredDocument{}, err
	}
	m.documentMu.Lock()
	defer m.documentMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return StoredDocument{}, err
	}
	if err := atomicfile.Write(path, append(encoded, '\n'), 0o640); err != nil {
		return StoredDocument{}, err
	}
	return stored, nil
}

// Document reads one stored document. A missing one is os.ErrNotExist.
func (m *Manager) Document(kind, name string) (StoredDocument, error) {
	path, err := m.documentPath(kind, name)
	if err != nil {
		return StoredDocument{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return StoredDocument{}, err
	}
	var stored StoredDocument
	if err := json.Unmarshal(raw, &stored); err != nil {
		return StoredDocument{}, fmt.Errorf("reading %s/%s: %w", kind, name, err)
	}
	return stored, nil
}

// Documents lists one kind's documents by name. A file that cannot be read is
// reported through the warning logger and skipped, as Campaigns does.
func (m *Manager) Documents(kind string) ([]StoredDocument, error) {
	if !storedKind(kind) {
		return nil, ErrNotStoredKind
	}
	entries, err := os.ReadDir(m.documentsDir(kind))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var documents []StoredDocument
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok || entry.IsDir() {
			continue
		}
		stored, err := m.Document(kind, name)
		if err != nil {
			m.Warnf("skipping unreadable %s document %q: %v", kind, entry.Name(), err)
			continue
		}
		documents = append(documents, stored)
	}
	sort.Slice(documents, func(i, j int) bool { return documents[i].Name < documents[j].Name })
	return documents, nil
}

// RemoveDocument deletes one stored document. A missing one is os.ErrNotExist.
func (m *Manager) RemoveDocument(kind, name string) error {
	path, err := m.documentPath(kind, name)
	if err != nil {
		return err
	}
	m.documentMu.Lock()
	defer m.documentMu.Unlock()
	return os.Remove(path)
}
