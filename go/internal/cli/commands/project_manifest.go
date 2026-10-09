package commands

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
)

// Keep the original JSON, including fields this CLI version does not understand.
// UseNumber also avoids rounding large integers in those fields during an edit.
type projectObject = map[string]any

type projectManifest struct {
	path     string
	original []byte
	mode     os.FileMode
	root     projectObject
	compose  *composeConfig
}

func parseProjectObject(data []byte) (projectObject, error) {
	if !json.Valid(data) {
		return nil, fmt.Errorf("manifest must contain valid JSON")
	}
	var obj projectObject
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := d.Decode(&obj); err != nil || obj == nil {
		return nil, fmt.Errorf("manifest must be a JSON object")
	}
	return obj, nil
}

func loadProjectManifest(path string) (*projectManifest, error) {
	if path == "" {
		path = "wendy.json"
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		path = filepath.Join(path, "wendy.json")
	}
	// Editing a symlink should update its target, not replace the link itself.
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	doc := &projectManifest{path: path, mode: 0o644, root: projectObject{}}
	for _, name := range []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(path), name)); err == nil {
			doc.compose, _, err = parseComposeFile(filepath.Dir(path))
			if err != nil {
				return nil, err
			}
			break
		}
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return doc, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	doc.mode, doc.original = info.Mode().Perm(), data
	doc.root, err = parseProjectObject(data)
	if err != nil {
		return doc, fmt.Errorf("%s: %w; use `wendy project edit --raw` to repair the JSON", path, err)
	}
	return doc, nil
}

func (d *projectManifest) requireExisting() error {
	if d.original == nil {
		return fmt.Errorf("no manifest at %s; run `wendy project` to create one for this project, or `wendy init` to scaffold a new app", d.path)
	}
	return nil
}

func (d *projectManifest) scope(service string) (projectObject, error) {
	if service == "" {
		return d.root, nil
	}
	services, _ := d.root["services"].(map[string]any)
	if raw, exists := services[service]; exists {
		obj, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("services.%s must be an object", service)
		}
		return obj, nil
	}
	if d.compose != nil {
		if _, exists := d.compose.Services[service]; exists {
			if services == nil {
				services = projectObject{}
				d.root["services"] = services
			}
			obj := projectObject{}
			services[service] = obj
			return obj, nil
		}
	}
	return nil, fmt.Errorf("unknown service %q; use `wendy project show` to see the services", service)
}

func (d *projectManifest) data() ([]byte, error) {
	data, err := json.MarshalIndent(d.root, "", "  ")
	return append(data, '\n'), err
}

func (d *projectManifest) validate() ([]string, error) {
	data, err := d.data()
	if err != nil {
		return nil, err
	}
	cfg, err := appconfig.LoadFromBytes(data)
	if err == nil {
		if d.compose != nil {
			err = cfg.ValidateComposeCompanion()
		} else {
			err = cfg.Validate()
		}
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", d.path, err)
	}
	return appconfig.ValidateJSON(data), nil
}

func (d *projectManifest) save() error {
	if _, err := d.validate(); err != nil {
		return err
	}
	current, err := os.ReadFile(d.path)
	if err != nil && !(os.IsNotExist(err) && d.original == nil) {
		return err
	}
	if !bytes.Equal(current, d.original) {
		return fmt.Errorf("%s changed while you were editing; reopen the project and try again", d.path)
	}
	data, err := d.data()
	if err != nil {
		return err
	}
	return atomicfile.Write(d.path, data, d.mode)
}

type projectChange struct {
	Path   string `json:"path"`
	Before any    `json:"before"`
	After  any    `json:"after"`
}

// Use JSON Pointer paths so service names and environment keys are unambiguous.
func projectChanges(before, after any, path string) []projectChange {
	if reflect.DeepEqual(before, after) {
		return nil
	}
	if a, ok := before.([]any); ok {
		if b, ok := after.([]any); ok {
			var changes []projectChange
			for i := 0; i < max(len(a), len(b)); i++ {
				var av, bv any
				if i < len(a) {
					av = a[i]
				}
				if i < len(b) {
					bv = b[i]
				}
				if av == nil && bv == nil && (i >= len(a) || i >= len(b)) {
					changes = append(changes, projectChange{Path: fmt.Sprintf("%s/%d", path, i)})
				} else {
					changes = append(changes, projectChanges(av, bv, fmt.Sprintf("%s/%d", path, i))...)
				}
			}
			return changes
		}
	}
	if a, ok := before.(map[string]any); ok || before == nil {
		if b, ok := after.(map[string]any); ok || after == nil {
			keys := map[string]bool{}
			for k := range a {
				keys[k] = true
			}
			for k := range b {
				keys[k] = true
			}
			names := make([]string, 0, len(keys))
			for k := range keys {
				names = append(names, k)
			}
			slices.Sort(names)
			var changes []projectChange
			for _, k := range names {
				child := strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1")
				av, aExists := a[k]
				bv, bExists := b[k]
				if aExists != bExists && av == nil && bv == nil {
					changes = append(changes, projectChange{Path: path + "/" + child})
				} else {
					changes = append(changes, projectChanges(av, bv, path+"/"+child)...)
				}
			}
			if len(names) > 0 {
				return changes
			}
		}
	}
	return []projectChange{{Path: path, Before: redactProjectValue(before, path), After: redactProjectValue(after, path)}}
}

func redactProjectValue(value any, path string) any {
	lower := strings.ToLower(path)
	if value != nil && (strings.Contains(lower, "password") || strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.HasSuffix(lower, "/env") || strings.Contains(lower, "/env/")) {
		return "<redacted>"
	}
	switch v := value.(type) {
	case map[string]any:
		out := projectObject{}
		for k, item := range v {
			out[k] = redactProjectValue(item, path+"/"+k)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = redactProjectValue(item, path)
		}
		return out
	}
	return value
}
