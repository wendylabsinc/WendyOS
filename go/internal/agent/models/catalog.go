package models

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
)

//go:embed catalog.json
var defaultCatalogJSON []byte

// Engines a variant can run on.
const (
	EngineTensorRT    = "tensorrt"
	EngineONNXRuntime = "onnxruntime"
	EngineQNN         = "qnn"
)

// KindDetector is the only model kind in this slice.
const KindDetector = "detector"

// Catalog is the set of models this agent can run.
type Catalog struct {
	Version string  `json:"version"`
	Models  []Model `json:"models"`
}

// Model is one catalog entry.
type Model struct {
	ID          string    `json:"id"`
	Description string    `json:"description"`
	Kind        string    `json:"kind"`
	Labels      []string  `json:"labels"`
	Variants    []Variant `json:"variants"`
}

// Variant is one way to run a model on a class of hardware.
type Variant struct {
	ID        string   `json:"id"`
	Engine    string   `json:"engine"`
	Requires  Requires `json:"requires"`
	HostImage string   `json:"host_image"`
	File      File     `json:"file"`
	InputSize int      `json:"input_size"`
}

// Requires holds predicates over a DeviceProfile; empty fields match anything.
type Requires struct {
	Arch           string `json:"arch,omitempty"`
	GPUVendor      string `json:"gpu_vendor,omitempty"`
	ComputeBackend string `json:"compute_backend,omitempty"`
	NPUBackend     string `json:"npu_backend,omitempty"`
}

// File is a content-addressed model file.
type File struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

var (
	catalogIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	labelPattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9 _-]{0,63}$`)
	digestRefPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.:/_-]*@sha256:[0-9a-f]{64}$`)
	sha256Pattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// DefaultCatalog returns the catalog compiled into the agent.
func DefaultCatalog() (Catalog, error) { return ParseCatalog(defaultCatalogJSON) }

// ParseCatalog decodes and validates a catalog document. Unknown fields are
// errors, so a typo cannot silently drop a requirement.
func ParseCatalog(raw []byte) (Catalog, error) {
	var c Catalog
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Catalog{}, fmt.Errorf("model catalog: %w", err)
	}
	if err := c.validate(); err != nil {
		return Catalog{}, fmt.Errorf("model catalog: %w", err)
	}
	return c, nil
}

// Model returns the entry with this id.
func (c Catalog) Model(id string) (Model, bool) {
	for _, m := range c.Models {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}

func (c Catalog) validate() error {
	if c.Version == "" {
		return errors.New("version is required")
	}
	seen := map[string]bool{}
	for _, m := range c.Models {
		if !catalogIDPattern.MatchString(m.ID) || seen[m.ID] {
			return fmt.Errorf("model id %q is invalid or repeated", m.ID)
		}
		seen[m.ID] = true
		if err := m.validate(); err != nil {
			return fmt.Errorf("model %s: %w", m.ID, err)
		}
	}
	return nil
}

func (m Model) validate() error {
	if m.Kind != KindDetector {
		return fmt.Errorf("kind %q is not supported", m.Kind)
	}
	if len(m.Labels) == 0 {
		return errors.New("labels are required")
	}
	labels := map[string]bool{}
	for _, l := range m.Labels {
		if !labelPattern.MatchString(l) || labels[l] {
			return fmt.Errorf("label %q is invalid or repeated", l)
		}
		labels[l] = true
	}
	if len(m.Variants) == 0 {
		return errors.New("at least one variant is required")
	}
	variants := map[string]bool{}
	for _, v := range m.Variants {
		if !catalogIDPattern.MatchString(v.ID) || variants[v.ID] {
			return fmt.Errorf("variant id %q is invalid or repeated", v.ID)
		}
		variants[v.ID] = true
		if err := v.validate(); err != nil {
			return fmt.Errorf("variant %s: %w", v.ID, err)
		}
	}
	return nil
}

func (v Variant) validate() error {
	switch v.Engine {
	case EngineTensorRT, EngineONNXRuntime, EngineQNN:
	default:
		return fmt.Errorf("engine %q is not supported", v.Engine)
	}
	// A digest is the only trust anchor for code the agent runs with camera
	// and accelerator access, so a tag is never enough.
	if !digestRefPattern.MatchString(v.HostImage) {
		return fmt.Errorf("host_image %q must be pinned by digest (name@sha256:…)", v.HostImage)
	}
	u, err := url.Parse(v.File.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("file url %q must be an https URL", v.File.URL)
	}
	if !sha256Pattern.MatchString(v.File.SHA256) {
		return fmt.Errorf("file sha256 %q is not 64 lowercase hex digits", v.File.SHA256)
	}
	if v.File.Bytes <= 0 {
		return errors.New("file bytes must be positive")
	}
	if v.InputSize <= 0 {
		return errors.New("input_size must be positive")
	}
	return nil
}
