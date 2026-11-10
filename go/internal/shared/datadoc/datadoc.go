// Package datadoc parses Wendy data documents: one-document YAML files that
// share a header (kind, version, name, owner) and are dispatched on kind.
//
// The package knows no kind itself. The packages that own the kinds build a
// Registry from them, so the CLI and the agent parse with the same rules.
// See specs/2026-10-12-data-documents-design.md §5.
package datadoc

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultKind is the kind of a document without a kind field: every campaign
// file written before data documents existed.
const DefaultKind = "collection"

// Header is what every data document starts with.
type Header struct {
	Kind    string
	Version int
	Name    string
	// Owner is "<kind>/<name>" of the document whose removal removes this
	// one, or empty.
	Owner string
}

// Document is a parsed data document. Body holds the kind's own type.
type Document struct {
	Header   Header
	Body     any
	Revision string
	Warnings []string
}

// Kind describes one kind of document.
type Kind struct {
	Name     string
	Versions []int
	// Ownable kinds may have an owner and may own other documents.
	Ownable bool
	// Decode parses and validates the whole document, strictly. The header
	// has already passed the registry's checks.
	Decode func(header Header, contents []byte) (body any, revision string, warnings []string, err error)
}

// Registry is the set of kinds one build understands.
type Registry struct {
	kinds map[string]Kind
}

// NewRegistry panics on a kind without a name, versions or decoder, and on a
// duplicate: those are programming errors, not input errors.
func NewRegistry(kinds ...Kind) *Registry {
	r := &Registry{kinds: map[string]Kind{}}
	for _, kind := range kinds {
		if kind.Name == "" || len(kind.Versions) == 0 || kind.Decode == nil {
			panic("datadoc: a kind needs a name, versions and a decoder")
		}
		if _, duplicate := r.kinds[kind.Name]; duplicate {
			panic("datadoc: kind " + kind.Name + " registered twice")
		}
		r.kinds[kind.Name] = kind
	}
	return r
}

// Kinds returns the registered kind names, sorted.
func (r *Registry) Kinds() []string {
	names := make([]string, 0, len(r.kinds))
	for name := range r.kinds {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

var nameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// rawHeader reads only the header fields and ignores the rest of the document.
// Version is decoded loosely because yaml.v3 reads 1.5 into an int as 1.
type rawHeader struct {
	Kind    string `yaml:"kind"`
	Version any    `yaml:"version"`
	Name    string `yaml:"name"`
	Owner   string `yaml:"owner"`
}

// ParseHeader checks a document's header without decoding its body.
func (r *Registry) ParseHeader(contents []byte) (Header, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(contents))
	var raw rawHeader
	if err := decoder.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return Header{}, errors.New("document is empty")
		}
		return Header{}, fmt.Errorf("parsing document header: %w", err)
	}
	var trailing any
	switch err := decoder.Decode(&trailing); {
	case err == nil:
		return Header{}, errors.New("a data document must contain exactly one YAML document")
	case !errors.Is(err, io.EOF):
		return Header{}, fmt.Errorf("a data document must contain exactly one YAML document; the text after the first one does not parse: %w", err)
	}
	version, ok := wholeNumber(raw.Version)
	if !ok {
		return Header{}, errors.New("version must be a whole number")
	}
	header := Header{Kind: raw.Kind, Version: version, Name: raw.Name, Owner: raw.Owner}
	if header.Kind == "" {
		header.Kind = DefaultKind
	}
	kind, ok := r.kinds[header.Kind]
	if !ok {
		return Header{}, fmt.Errorf("unknown kind %q; known kinds: %s", header.Kind, strings.Join(r.Kinds(), ", "))
	}
	versions := joinInts(kind.Versions)
	if header.Version == 0 {
		return Header{}, fmt.Errorf("version is required (%s documents support version %s)", header.Kind, versions)
	}
	if !slices.Contains(kind.Versions, header.Version) {
		return Header{}, fmt.Errorf("%s version %d is not supported (supported: %s)", header.Kind, header.Version, versions)
	}
	if header.Name == "" {
		return Header{}, errors.New("name is required")
	}
	if !nameRE.MatchString(header.Name) {
		return Header{}, errors.New("name must use 1 to 128 letters, numbers, '.', '-' or '_'")
	}
	if header.Owner != "" {
		if err := r.checkOwner(kind, header); err != nil {
			return Header{}, err
		}
	}
	return header, nil
}

// Parse checks the header, then decodes the whole document with its kind.
func (r *Registry) Parse(contents []byte) (Document, error) {
	header, err := r.ParseHeader(contents)
	if err != nil {
		return Document{}, err
	}
	body, revision, warnings, err := r.kinds[header.Kind].Decode(header, contents)
	if err != nil {
		return Document{}, err
	}
	return Document{Header: header, Body: body, Revision: revision, Warnings: warnings}, nil
}

func (r *Registry) checkOwner(kind Kind, header Header) error {
	if !kind.Ownable {
		return fmt.Errorf("%s documents cannot have an owner", kind.Name)
	}
	var ownable []string
	for _, name := range r.Kinds() {
		if r.kinds[name].Ownable {
			ownable = append(ownable, name)
		}
	}
	invalid := fmt.Errorf("owner %q must be <kind>/<name> naming a document of kind %s", header.Owner, strings.Join(ownable, " or "))
	ownerKind, ownerName, found := strings.Cut(header.Owner, "/")
	if !found || !slices.Contains(ownable, ownerKind) || !nameRE.MatchString(ownerName) {
		return invalid
	}
	if ownerKind == header.Kind && ownerName == header.Name {
		return errors.New("a document cannot own itself")
	}
	return nil
}

// wholeNumber accepts an absent version (0), an int, or a float with no
// fractional part, such as 1.0.
func wholeNumber(value any) (int, bool) {
	switch v := value.(type) {
	case nil:
		return 0, true
	case int:
		return v, true
	case float64:
		if v == math.Trunc(v) && v >= math.MinInt32 && v <= math.MaxInt32 {
			return int(v), true
		}
	}
	return 0, false
}

func joinInts(values []int) string {
	parts := make([]string, len(values))
	for i, value := range values {
		parts[i] = strconv.Itoa(value)
	}
	return strings.Join(parts, ", ")
}
