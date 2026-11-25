package go2executor

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
)

// The old decoder is kept only as a differential test oracle. Both paths must
// first see the same strict root pass, which owns duplicate/depth/trailing checks.
func rootCheckedCloudForTest(raw []byte, legacy bool) (wireCloud, error) {
	var fields map[string]json.RawMessage
	var cloud wireCloud
	if err := authority.DecodeStrictLimit(raw, &fields, maxResponse); err != nil {
		return cloud, err
	}
	if legacy {
		err := requiredObjectLimit(fields["cloud"], &cloud, maxResponse)
		return cloud, err
	}
	err := decodeRootCheckedCloud(fields["cloud"], &cloud)
	return cloud, err
}

func TestCloudDecodeRootCheckedMatchesLegacy(t *testing.T) {
	c, _, a, _ := cloudController(t)
	s, err := a.State(context.Background(), c.session)
	if err != nil {
		t.Fatal(err)
	}
	s.Cloud.Ranges[0] = math.Inf(1)
	s.Cloud.Ranges[1] = math.Nextafter(.1, 1)
	raw := cloudStateJSON(t, s)
	var top map[string]json.RawMessage
	_ = json.Unmarshal(raw, &top)
	var original map[string]json.RawMessage
	_ = json.Unmarshal(top["cloud"], &original)
	cases := map[string][]byte{"valid": raw}
	add := func(name string, cloud []byte) {
		cases[name] = append(append([]byte(`{"cloud":`), cloud...), '}')
	}
	for key := range original {
		for _, mode := range []string{"missing", "null", "object", "case-alias"} {
			var changed map[string]json.RawMessage
			_ = json.Unmarshal(top["cloud"], &changed)
			switch mode {
			case "missing":
				delete(changed, key)
			case "null":
				changed[key] = json.RawMessage("null")
			case "object":
				changed[key] = json.RawMessage(`{}`)
			case "case-alias":
				changed[strings.ToUpper(key)] = changed[key]
				delete(changed, key)
			}
			value, _ := json.Marshal(changed)
			add(key+"-"+mode, value)
		}
	}
	for name, replacement := range map[string][]byte{
		"range-boolean": []byte(`true`), "range-string": []byte(`"1"`),
		"range-object": []byte(`{}`), "range-overflow": []byte(`1e999`),
		"range-nested": []byte(strings.Repeat("[", 34) + "1" + strings.Repeat("]", 34)),
	} {
		add(name, bytes.Replace(top["cloud"], []byte(`"ranges":[null,`), append(append([]byte(`"ranges":[`), replacement...), ','), 1))
	}
	add("duplicate-field", append([]byte(`{"model":"duplicate",`), top["cloud"][1:]...))
	add("escaped-duplicate", append([]byte(`{"mo\u0064el":"duplicate",`), top["cloud"][1:]...))
	add("unknown-field", append([]byte(`{"unknown":1,`), top["cloud"][1:]...))
	cases["trailing"] = append(append([]byte(nil), raw...), []byte(` {}`)...)
	cases["oversize"] = append(append([]byte(nil), raw...), bytes.Repeat([]byte(" "), maxResponse)...)
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			old, oldErr := rootCheckedCloudForTest(value, true)
			got, err := rootCheckedCloudForTest(value, false)
			if (err == nil) != (oldErr == nil) || err == nil && !reflect.DeepEqual(old, got) {
				t.Fatalf("legacy acceptance/value differs: old=%v new=%v", oldErr, err)
			}
			if name != "valid" && err == nil {
				t.Fatal("adversarial input accepted")
			}
		})
	}
	decoded, err := DecodeState(raw)
	if err != nil || !reflect.DeepEqual(decoded.Cloud, s.Cloud) {
		t.Fatalf("full decoder changed a range, missing return or original metadata: %v", err)
	}
}
