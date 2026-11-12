package litebuild

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Attribute types.
const (
	TypeString  = "string"
	TypeSize    = "size"
	TypeBoolean = "boolean"
)

// Sdkconfig is the intrinsic node that collects the sdkconfig lines.
const Sdkconfig = "sdkconfig"

// DisplaySection is the top-level key of the display information.
const DisplaySection = "display"

// FormatVersions lists the format_version values that this code reads.
var FormatVersions = []string{"1.0"}

var types = []string{TypeString, TypeSize, TypeBoolean}

var sizeUnits = []struct {
	name string
	mult int64
}{
	{"GiB", 1 << 30},
	{"MiB", 1 << 20},
	{"KiB", 1 << 10},
}

var (
	sizeRe = regexp.MustCompile(`^\s*(\d+)\s*(KiB|MiB|GiB)?\s*$`)
	nameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// ConfError is an error in the master configuration or in the values.
type ConfError struct {
	Msg string
}

func (e *ConfError) Error() string { return e.Msg }

func confErrorf(format string, args ...any) error {
	return &ConfError{Msg: fmt.Sprintf(format, args...)}
}

// where adds a location prefix to a ConfError, as _where in conf_model.py.
func where(location string, err error) error {
	if err == nil {
		return nil
	}
	if ce, ok := err.(*ConfError); ok {
		return &ConfError{Msg: location + ": " + ce.Msg}
	}
	return err
}

// A value is a string, an int64 (a size in bytes) or a bool. nil is
// undefined.

// ParseSize returns a size in bytes. It accepts an integer or a text such
// as "8MiB".
func ParseSize(raw any) (int64, error) {
	switch v := raw.(type) {
	case int64:
		if v >= 0 {
			return v, nil
		}
	case int:
		if v >= 0 {
			return int64(v), nil
		}
	case string:
		if m := sizeRe.FindStringSubmatch(v); m != nil {
			n, err := strconv.ParseInt(m[1], 10, 64)
			if err == nil {
				mult := int64(1)
				for _, u := range sizeUnits {
					if u.name == m[2] {
						mult = u.mult
					}
				}
				return n * mult, nil
			}
		}
	}
	return 0, confErrorf("%s is not a size (use a number with an optional KiB, MiB or GiB unit)", pyRepr(raw))
}

// ParseBool returns a boolean. It accepts a bool, 0/1, or true/false,
// yes/no, on/off.
func ParseBool(raw any) (bool, error) {
	switch v := raw.(type) {
	case bool:
		return v, nil
	case int64:
		if v == 0 || v == 1 {
			return v == 1, nil
		}
	case int:
		if v == 0 || v == 1 {
			return v == 1, nil
		}
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "yes", "on", "1":
			return true, nil
		case "false", "no", "off", "0":
			return false, nil
		}
	}
	return false, confErrorf("%s is not a boolean (use true or false)", pyRepr(raw))
}

// pyRepr formats a raw value as Python repr() does, so that the messages
// match the Python configurator.
func pyRepr(raw any) string {
	switch v := raw.(type) {
	case string:
		return "'" + v + "'"
	case bool:
		if v {
			return "True"
		}
		return "False"
	case nil:
		return "None"
	default:
		return fmt.Sprint(v)
	}
}

// Display is the display information of an attribute.
type Display struct {
	Title    string
	Help     string
	Priority *int
	Default  any
	// Labels maps a value (as returned by Attribute.Key) to its label.
	Labels map[string]string
}

// Attribute is one value of the configuration.
type Attribute struct {
	Name  string
	Type  string
	Enum  []any // nil: no enum
	Index int   // declaration order
	// Visible is true when the attribute has an entry in the display
	// section. An attribute that is not visible is internal: the CLI never
	// shows it.
	Visible bool
	Display Display
}

// Convert converts a YAML value to the attribute type, without the enum
// check.
func (a *Attribute) Convert(raw any) (any, error) {
	switch a.Type {
	case TypeSize:
		return ParseSize(raw)
	case TypeBoolean:
		return ParseBool(raw)
	}
	if s, ok := raw.(string); ok {
		return s, nil
	}
	return nil, confErrorf("%s is not a string (put quotes around it)", pyRepr(raw))
}

// Normalize converts a YAML value to the attribute type and checks the
// enum.
func (a *Attribute) Normalize(raw any) (any, error) {
	value, err := a.Convert(raw)
	if err != nil {
		return nil, where(a.Name, err)
	}
	if a.Enum != nil && !a.inEnum(value) {
		allowed := make([]string, len(a.Enum))
		for i, v := range a.Enum {
			allowed[i] = a.Format(v)
		}
		return nil, where(a.Name, confErrorf("%s is not one of: %s", a.Format(value), strings.Join(allowed, ", ")))
	}
	return value, nil
}

func (a *Attribute) inEnum(value any) bool {
	for _, v := range a.Enum {
		if v == value {
			return true
		}
	}
	return false
}

// Format returns the value as the Python configurator prints it.
func (a *Attribute) Format(value any) string {
	if value == nil {
		return "<undefined>"
	}
	switch v := value.(type) {
	case int64:
		for _, u := range sizeUnits {
			if v != 0 && v%u.mult == 0 {
				return fmt.Sprintf("%d (%d %s)", v, v/u.mult, u.name)
			}
		}
		return strconv.FormatInt(v, 10)
	case bool:
		return strconv.FormatBool(v)
	}
	return fmt.Sprint(value)
}

// Key returns a short text of the value: the text of a NAME=VALUE argument
// ("8MiB", "true", "seeed_xiao"). The display labels use it as their key.
func (a *Attribute) Key(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case int64:
		return sizeText(v)
	case bool:
		return strconv.FormatBool(v)
	}
	return fmt.Sprint(value)
}

// Label returns the human text of a value: its display label, else a
// readable form of the value.
func (a *Attribute) Label(value any) string {
	if label, ok := a.Display.Labels[a.Key(value)]; ok {
		return label
	}
	switch v := value.(type) {
	case nil:
		return "<undefined>"
	case int64:
		for _, u := range sizeUnits {
			if v != 0 && v%u.mult == 0 {
				return fmt.Sprintf("%d %s", v/u.mult, u.name)
			}
		}
		return strconv.FormatInt(v, 10)
	case bool:
		if v {
			return "yes"
		}
		return "no"
	}
	return fmt.Sprint(value)
}

// Title returns the question title of the attribute.
func (a *Attribute) Title() string {
	if a.Display.Title != "" {
		return a.Display.Title
	}
	return a.Name
}

// sizeText gives a size with the largest unit that divides it ("8MiB"),
// else a number of bytes, as Attribute.to_yaml in conf_model.py.
func sizeText(n int64) string {
	for _, u := range sizeUnits {
		if n != 0 && n%u.mult == 0 {
			return fmt.Sprintf("%d%s", n/u.mult, u.name)
		}
	}
	return strconv.FormatInt(n, 10)
}
