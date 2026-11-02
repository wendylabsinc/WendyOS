package jobops

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// ParseSpec keeps CLI, file-reference and inline MCP validation identical.
func ParseSpec(data []byte) (Spec, error) {
	var spec Spec
	if len(data) > 120000 {
		return spec, fmt.Errorf("job spec exceeds 120000 bytes")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return spec, fmt.Errorf("invalid job spec: %w", err)
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return spec, fmt.Errorf("job spec must contain exactly one JSON object")
	}
	return spec, ValidateSpec(spec)
}

// LoadSpec reads a local regular JSON file without copying its contents into
// the model conversation. The path is local to the CLI/MCP server, not the host.
func LoadSpec(name string) (Spec, error) {
	data, err := readLocalSpec(name)
	if err != nil {
		return Spec{}, err
	}
	return ParseSpec(data)
}

func readLocalSpec(name string) ([]byte, error) {
	info, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 120000 {
		return nil, fmt.Errorf("job spec must be a regular file of at most 120000 bytes")
	}
	f, err := openSpec(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() {
		return nil, fmt.Errorf("job spec changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, 120001))
	if err != nil {
		return nil, err
	}
	if len(data) > 120000 {
		return nil, fmt.Errorf("job spec exceeds 120000 bytes")
	}
	return data, nil
}
