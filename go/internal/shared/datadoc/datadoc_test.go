package datadoc

import (
	"errors"
	"strings"
	"testing"
)

type testBody struct{ contents string }

func testRegistry(decodeErr error) *Registry {
	decode := func(header Header, contents []byte) (any, string, []string, error) {
		if decodeErr != nil {
			return nil, "", nil, decodeErr
		}
		return testBody{string(contents)}, "rev-" + header.Kind + "-" + header.Name, []string{"warned"}, nil
	}
	return NewRegistry(
		Kind{Name: "collection", Versions: []int{1}, Decode: decode},
		Kind{Name: "inference", Versions: []int{1}, Ownable: true, Decode: decode},
		Kind{Name: "watch", Versions: []int{1, 2}, Ownable: true, Decode: decode},
	)
}

func TestParseDefaultsToCollectionAndPassesTheWholeDocument(t *testing.T) {
	doc, err := testRegistry(nil).Parse([]byte("version: 1\nname: people\nsources: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	if doc.Header != (Header{Kind: "collection", Version: 1, Name: "people"}) {
		t.Fatalf("header = %+v", doc.Header)
	}
	if doc.Revision != "rev-collection-people" || len(doc.Warnings) != 1 || doc.Warnings[0] != "warned" {
		t.Fatalf("document = %+v", doc)
	}
	if body, ok := doc.Body.(testBody); !ok || !strings.Contains(body.contents, "sources: []") {
		t.Fatalf("the decoder did not get the whole document: %+v", doc.Body)
	}
}

func TestParseAcceptsAnOwner(t *testing.T) {
	doc, err := testRegistry(nil).Parse([]byte("kind: inference\nversion: 1\nname: door\nowner: watch/door\n"))
	if err != nil {
		t.Fatal(err)
	}
	if doc.Header != (Header{Kind: "inference", Version: 1, Name: "door", Owner: "watch/door"}) {
		t.Fatalf("header = %+v", doc.Header)
	}
}

func TestParseAcceptsAWholeFloatVersion(t *testing.T) {
	// yaml reads 1.0 as a float; it names version 1 exactly.
	header, err := testRegistry(nil).ParseHeader([]byte("version: 1.0\nname: x\n"))
	if err != nil || header.Version != 1 {
		t.Fatalf("version 1.0 = %+v, %v", header, err)
	}
}

func TestParseHeaderErrors(t *testing.T) {
	cases := []struct{ name, doc, want string }{
		{"empty", "", "document is empty"},
		{"comment only", "# nothing\n", "document is empty"},
		{"not a mapping", "- a\n- b\n", "a data document is a YAML mapping"},
		{"unknown kind", "kind: wach\nversion: 1\nname: x\n", `unknown kind "wach"; known kinds: collection, inference, watch`},
		{"version missing", "kind: watch\nname: x\n", "version is required (watch documents support version 1, 2)"},
		{"version zero", "version: 0\nname: x\n", "version is required (collection documents support version 1)"},
		{"version unsupported", "kind: inference\nversion: 2\nname: x\n", "inference version 2 is not supported (supported: 1)"},
		{"version fractional", "version: 1.5\nname: x\n", "version must be a whole number"},
		{"version quoted", "version: \"1\"\nname: x\n", "version must be a whole number"},
		{"version a word", "version: one\nname: x\n", "version must be a whole number"},
		{"name missing", "version: 1\n", "name is required"},
		{"name with a space", "version: 1\nname: front door\n", "name must use 1 to 128 letters, numbers, '.', '-' or '_'"},
		{"name too long", "version: 1\nname: " + strings.Repeat("a", 129) + "\n", "name must use 1 to 128 letters"},
		{"second document", "version: 1\nname: x\n---\nversion: 1\nname: y\n", "must contain exactly one YAML document"},
		{"broken second document", "version: 1\nname: x\n---\n[unclosed\n", "the text after the first one does not parse"},
		{"owner on collection", "version: 1\nname: x\nowner: watch/w\n", "collection documents cannot have an owner"},
		{"owner without slash", "kind: inference\nversion: 1\nname: x\nowner: w\n", `owner "w" must be <kind>/<name> naming a document of kind inference or watch`},
		{"owner of unknown kind", "kind: inference\nversion: 1\nname: x\nowner: robot/w\n", `owner "robot/w" must be <kind>/<name>`},
		{"owner is a collection", "kind: inference\nversion: 1\nname: x\nowner: collection/c\n", `owner "collection/c" must be <kind>/<name>`},
		{"owner name invalid", "kind: inference\nversion: 1\nname: x\nowner: watch/a b\n", `owner "watch/a b" must be <kind>/<name>`},
		{"owns itself", "kind: watch\nversion: 1\nname: x\nowner: watch/x\n", "a document cannot own itself"},
	}
	registry := testRegistry(nil)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := registry.ParseHeader([]byte(tc.doc))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
			if _, parseErr := registry.Parse([]byte(tc.doc)); parseErr == nil || parseErr.Error() != err.Error() {
				t.Fatalf("Parse error = %v, want the header error %v", parseErr, err)
			}
		})
	}
}

func TestParseHeaderDoesNotDecodeTheBody(t *testing.T) {
	registry := testRegistry(errors.New("decoder ran"))
	if _, err := registry.ParseHeader([]byte("kind: watch\nversion: 2\nname: x\n")); err != nil {
		t.Fatalf("ParseHeader ran the decoder: %v", err)
	}
}

func TestParseReturnsTheDecoderError(t *testing.T) {
	want := errors.New("lease must be at least 15s")
	_, err := testRegistry(want).Parse([]byte("kind: watch\nversion: 1\nname: x\n"))
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want the decoder's", err)
	}
}

func TestNewRegistryRefusesBadKinds(t *testing.T) {
	decode := func(Header, []byte) (any, string, []string, error) { return nil, "", nil, nil }
	for name, kinds := range map[string][]Kind{
		"no name":     {{Versions: []int{1}, Decode: decode}},
		"no versions": {{Name: "watch", Decode: decode}},
		"no decoder":  {{Name: "watch", Versions: []int{1}}},
		"duplicate":   {{Name: "watch", Versions: []int{1}, Decode: decode}, {Name: "watch", Versions: []int{1}, Decode: decode}},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("NewRegistry accepted it")
				}
			}()
			NewRegistry(kinds...)
		})
	}
}

func TestKindsAreSorted(t *testing.T) {
	if got := strings.Join(testRegistry(nil).Kinds(), ","); got != "collection,inference,watch" {
		t.Fatalf("Kinds() = %s", got)
	}
}

func TestHeaderTypeErrorsNameTheField(t *testing.T) {
	registry := testRegistry(nil)
	for doc, want := range map[string]string{
		"kind: [a]\nversion: 1\nname: x\n":                   "kind must be a string",
		"version: 1\nname: {a: b}\n":                         "name must be a string",
		"kind: inference\nversion: 1\nname: x\nowner: [w]\n": "owner must be a string",
		"version: [1]\nname: x\n":                            "version must be a whole number",
		"- a\n- b\n":                                         "a data document is a YAML mapping",
	} {
		if _, err := registry.ParseHeader([]byte(doc)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error = %v, want %q", doc, err, want)
		}
	}
}

func TestNewRegistryRefusesUnnameableKinds(t *testing.T) {
	decode := func(Header, []byte) (any, string, []string, error) { return nil, "", nil, nil }
	for _, name := range []string{"a/b", "has space", strings.Repeat("k", 129)} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("NewRegistry accepted kind %q", name)
				}
			}()
			NewRegistry(Kind{Name: name, Versions: []int{1}, Decode: decode})
		}()
	}
}
