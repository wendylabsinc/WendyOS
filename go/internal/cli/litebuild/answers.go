package litebuild

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// LoadAnswers reads the values that SaveAnswers wrote, for
// Configurator.Previous. The file is a JSON object of NAME: VALUE texts, the
// VALUE of a NAME=VALUE argument ({"board": "seeed_xiao", "flash_size":
// "8MiB"}).
//
// The master configuration can change between runs, so an unknown attribute
// or a value that is not valid for the attribute is skipped. A missing or
// unreadable file gives no values: the answers only give defaults.
func LoadAnswers(m *Model, path string) map[string]any {
	values := map[string]any{}
	data, err := os.ReadFile(path)
	if err != nil {
		return values
	}
	var texts map[string]string
	if err := json.Unmarshal(data, &texts); err != nil {
		return values
	}
	for name, text := range texts {
		attr := m.Attributes[name]
		if attr == nil {
			continue
		}
		if v, err := attr.Normalize(text); err == nil {
			values[name] = v
		}
	}
	return values
}

// SaveAnswers writes previous merged with inputs (inputs win) to path, for
// LoadAnswers. The merge keeps the answer to a question that this run did
// not ask, for the next run that asks it. Unknown attributes are dropped.
func SaveAnswers(m *Model, path string, previous, inputs map[string]any) error {
	texts := map[string]string{}
	for _, values := range []map[string]any{previous, inputs} {
		for name, v := range values {
			if attr := m.Attributes[name]; attr != nil && v != nil {
				texts[name] = attr.Key(v)
			}
		}
	}
	data, err := json.MarshalIndent(texts, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("saving the answers: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("saving the answers: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op after the rename
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("saving the answers: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("saving the answers: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("saving the answers: %w", err)
	}
	return nil
}
