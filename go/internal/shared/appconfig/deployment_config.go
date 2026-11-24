package appconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode"
)

// MaxDeploymentConfigSize caps DeviceAssignment.config_json.
const MaxDeploymentConfigSize = 64 << 10

// CheckDeploymentConfig is the device's check of a signed configuration (a
// deployment's config_json), which the CLI also runs before signing one: non-empty,
// exactly one JSON object, at most 64 KiB.
func CheckDeploymentConfig(config string) error {
	if config == "" {
		return errors.New("the configuration is empty")
	}
	if len(config) > MaxDeploymentConfigSize {
		return fmt.Errorf("the configuration is %d bytes, over the %d byte limit", len(config), MaxDeploymentConfigSize)
	}
	dec := json.NewDecoder(strings.NewReader(config))
	dec.UseNumber() // a number's text is kept: 1e400 is valid JSON
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return errors.New("the configuration is not a JSON object")
	}
	if err := checkJSONObject(dec); err != nil {
		return fmt.Errorf("the configuration is not valid JSON: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("the configuration is more than one JSON object")
	}
	// Named values only: host access comes from the release, never from a
	// deployment (sem, 2026-11-12).
	_, err := DeploymentConfigEnv(config)
	return err
}

// checkJSONObject reads the rest of an object whose '{' was read, refusing a
// key that appears twice in it or in any object nested inside it: which of
// two values counts would depend on the parser. Keys equal ignoring case
// count as the same key, because Go's JSON decoder (the start step's) matches
// field names without regard to case, so the last of them would win.
func checkJSONObject(dec *json.Decoder) error {
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := tok.(string)
		folded := foldKey(key)
		if seen[folded] {
			return fmt.Errorf("key %q appears twice", key)
		}
		seen[folded] = true
		if err := checkJSONValue(dec); err != nil {
			return err
		}
	}
	_, err := dec.Token() // '}'
	return err
}

// foldKey maps every rune to the smallest rune of its case-fold orbit, so
// foldKey(a) == foldKey(b) exactly when strings.EqualFold(a, b).
func foldKey(s string) string {
	return strings.Map(func(r rune) rune {
		least := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			least = min(least, f)
		}
		return least
	}, s)
}

func checkJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch tok {
	case json.Delim('{'):
		return checkJSONObject(dec)
	case json.Delim('['):
		for dec.More() {
			if err := checkJSONValue(dec); err != nil {
				return err
			}
		}
		_, err = dec.Token() // ']'
	}
	return err
}

// DeploymentConfigEnv turns a configuration (checked by CheckDeploymentConfig) into environment
// entries, sorted by name.
func DeploymentConfigEnv(config string) ([]string, error) {
	dec := json.NewDecoder(strings.NewReader(config))
	dec.UseNumber()
	var values map[string]any
	if err := dec.Decode(&values); err != nil {
		return nil, err
	}
	env := make([]string, 0, len(values))
	for name, v := range values {
		// Checked here as well: ValidateEnvEntries reads a name only up to its
		// first "=", so "A=B" would pass there as A.
		if !IsEnvVarName(name) {
			return nil, fmt.Errorf("configuration name %q is not a valid environment variable name (letters, digits and _, not starting with a digit)", name)
		}
		var text string
		switch v := v.(type) {
		case string:
			text = v
		case json.Number:
			text = v.String()
		case bool:
			text = fmt.Sprint(v)
		default:
			return nil, fmt.Errorf("the configuration may hold only named values (text, numbers, true or false); %q holds more", name)
		}
		if IsAgentEnvName(name) {
			return nil, fmt.Errorf("configuration name %q is set by the agent itself", name)
		}
		env = append(env, name+"="+text)
	}
	slices.Sort(env)
	// At least as strict as the runtime, so a statement is refused when it
	// arrives rather than failing at start: no reserved names (the agent's
	// own or the linkers'), no NUL, CR or LF, entries of at most
	// 32 KiB, at most 512 of them.
	if err := ValidateEnvEntries(env); err != nil {
		return nil, fmt.Errorf("the configuration cannot be applied: %w", err)
	}
	return env, nil
}
