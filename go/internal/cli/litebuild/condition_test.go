package litebuild

import (
	"strings"
	"testing"
)

func testAttributes() map[string]*Attribute {
	return map[string]*Attribute{
		"board": {Name: "board", Type: TypeString, Enum: []any{"a", "b"}},
		"name":  {Name: "name", Type: TypeString},
		"flash": {Name: "flash", Type: TypeSize},
		"net":   {Name: "net", Type: TypeBoolean},
	}
}

func TestParseCondition(t *testing.T) {
	values := map[string]any{"board": "a", "flash": int64(8 << 20), "net": true}
	tests := []struct {
		text, str string
		want      bool
	}{
		{`board == "a"`, `board == "a"`, true},
		{`board != 'b'`, `board != "b"`, true},
		{`net`, `net`, true},
		{`!net`, `!net`, false},
		{`net == false`, `net == false`, false},
		{`flash >= 8MiB && flash < 16 MiB`, `flash >= 8388608 (8 MiB) && flash < 16777216 (16 MiB)`, true},
		{`board == "b" || net && flash > 0`, `board == "b" || (net && flash > 0)`, true},
		{`!(board == "a" || net)`, `!(board == "a" || net)`, false},
		{`!!net`, `!!net`, true},
		// Undefined: only != is true, a boolean alone is false.
		{`name == "x"`, `name == "x"`, false},
		{`name != "x"`, `name != "x"`, true},
		{`!name_set_test`, ``, false},
	}
	attrs := testAttributes()
	attrs["name_set_test"] = &Attribute{Name: "name_set_test", Type: TypeBoolean}
	for _, tc := range tests {
		node, err := ParseCondition(tc.text, attrs)
		if err != nil {
			t.Errorf("%s: %v", tc.text, err)
			continue
		}
		if tc.str != "" && node.String() != tc.str {
			t.Errorf("%s: String() = %s, want %s", tc.text, node.String(), tc.str)
		}
		if tc.text == `!name_set_test` {
			// An undefined boolean alone is false, so its negation is true.
			if !node.Eval(values) {
				t.Errorf("%s: Eval = false, want true", tc.text)
			}
			continue
		}
		if got := node.Eval(values); got != tc.want {
			t.Errorf("%s: Eval = %v, want %v", tc.text, got, tc.want)
		}
	}
}

func TestParseConditionErrors(t *testing.T) {
	tests := []struct{ text, want string }{
		{`chip == "a"`, `unknown attribute "chip" at column 1`},
		{`board < "a"`, `< needs a size attribute, board is a string`},
		{`board == "c"`, `board: c is not one of: a, b`},
		{`board == a`, `expected a quoted string after board ==, found "a" at column 10`},
		{`flash == "8MiB"`, `expected a size such as 8MiB after flash ==, found ""8MiB"" at column 10`},
		{`net == yes`, `expected true or false after net ==, found "yes" at column 8`},
		{`board`, `expected a comparison after board, found the end`},
		{`(net`, `expected ), found the end`},
		{`net net`, `expected && or || or the end, found "net" at column 5`},
		{`net # x`, `unexpected "#" at column 5`},
		{`sdkconfig`, `sdkconfig cannot be used in a condition`},
	}
	for _, tc := range tests {
		_, err := ParseCondition(tc.text, testAttributes())
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want %q", tc.text, err, tc.want)
		}
	}
}

func TestParseSizeAndBool(t *testing.T) {
	for raw, want := range map[any]int64{"8MiB": 8 << 20, " 4 KiB ": 4096, int64(12): 12, "0": 0, "1GiB": 1 << 30} {
		if got, err := ParseSize(raw); err != nil || got != want {
			t.Errorf("ParseSize(%v) = %d, %v, want %d", raw, got, err, want)
		}
	}
	for _, raw := range []any{"8MB", int64(-1), "x", true} {
		if _, err := ParseSize(raw); err == nil {
			t.Errorf("ParseSize(%v): no error", raw)
		}
	}
	for raw, want := range map[any]bool{"yes": true, "Off": false, int64(1): true, false: false, "0": false} {
		if got, err := ParseBool(raw); err != nil || got != want {
			t.Errorf("ParseBool(%v) = %v, %v, want %v", raw, got, err, want)
		}
	}
	if _, err := ParseBool("maybe"); err == nil || err.Error() != "'maybe' is not a boolean (use true or false)" {
		t.Errorf("ParseBool(maybe) error = %v", err)
	}
}
