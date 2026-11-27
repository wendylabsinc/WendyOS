package go2executor

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/go2nav"
)

func cloudDecodeReviewFixture(t *testing.T) (*Controller, *testClock, []byte) {
	t.Helper()
	c, _, adapter, clock := cloudController(t)
	s, err := adapter.State(context.Background(), c.session)
	if err != nil {
		t.Fatal(err)
	}
	s.Cloud.Ranges[0] = math.Inf(1)
	return c, clock, cloudStateJSON(t, s)
}

func TestHTTPCloudDecodeReviewRootFences(t *testing.T) {
	_, _, raw := cloudDecodeReviewFixture(t)
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	cloud := top["cloud"]
	replace := func(value []byte) []byte { return bytes.Replace(raw, cloud, value, 1) }
	cases := map[string][]byte{
		"root-duplicate":          append(append(append([]byte(nil), raw[:len(raw)-1]...), []byte(`,"cloud":`)...), append(cloud, '}')...),
		"escaped-cloud-duplicate": replace(append([]byte(`{"mo\u0064el":"ambiguous",`), cloud[1:]...)),
		"nested-duplicate":        replace(bytes.Replace(cloud, []byte(`"ranges":[null,`), []byte(`"ranges":[{"x":1,"\u0078":2},`), 1)),
		"nested-depth":            replace(bytes.Replace(cloud, []byte(`"ranges":[null,`), []byte(`"ranges":[`+strings.Repeat("[", 33)+"1"+strings.Repeat("]", 33)+","), 1)),
		"trailing-object":         append(append([]byte(nil), raw...), []byte(` {}`)...),
		"oversize-whitespace":     append(append([]byte(nil), raw...), bytes.Repeat([]byte(" "), maxResponse-len(raw)+1)...),
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeState(value); err == nil {
				t.Fatal("whole-body strict fence was bypassed")
			}
		})
	}
	// The advertised exact byte bound still permits harmless trailing whitespace.
	atLimit := append(append([]byte(nil), raw...), bytes.Repeat([]byte(" "), maxResponse-len(raw))...)
	if _, err := DecodeState(atLimit); err != nil {
		t.Fatalf("exact body bound rejected: %v", err)
	}
}

func TestHTTPCloudDecodeReviewNumericAndAssociationBounds(t *testing.T) {
	_, _, raw := cloudDecodeReviewFixture(t)
	for _, kind := range []string{"fractional-sequence", "quoted-stamp", "uint-overflow", "epoch-boolean", "short-ranges", "negative-range", "above-maximum", "companion-mismatch", "zero-publisher", "unknown-instead-of-required"} {
		t.Run(kind, func(t *testing.T) {
			var top, cloud map[string]json.RawMessage
			_ = json.Unmarshal(raw, &top)
			_ = json.Unmarshal(top["cloud"], &cloud)
			switch kind {
			case "fractional-sequence":
				cloud["sequence"] = json.RawMessage(`1.0`)
			case "quoted-stamp":
				cloud["captured_unix_ns"] = json.RawMessage(`"1800000000000000000"`)
			case "uint-overflow":
				cloud["sequence"] = json.RawMessage(`18446744073709551616`)
			case "epoch-boolean":
				cloud["observed_epoch"] = json.RawMessage(`true`)
			case "short-ranges":
				cloud["ranges"] = json.RawMessage(`[null]`)
			case "negative-range", "above-maximum", "companion-mismatch":
				var ranges []*float64
				_ = json.Unmarshal(cloud["ranges"], &ranges)
				i, value := 0, -1.
				if kind == "above-maximum" {
					value = math.Nextafter(12+go2nav.CloudRangeQuantizationM, math.Inf(1))
				}
				if kind == "companion-mismatch" {
					i, value = 12*go2nav.CloudAzimuths, 2.
				}
				ranges[i] = &value
				cloud["ranges"], _ = json.Marshal(ranges)
			case "zero-publisher":
				cloud["publisher_gid"], _ = json.Marshal(strings.Repeat("0", 48))
			case "unknown-instead-of-required":
				cloud["unrecognized"] = cloud["frame"]
				delete(cloud, "frame")
			}
			top["cloud"], _ = json.Marshal(cloud)
			changed, _ := json.Marshal(top)
			if _, err := DecodeState(changed); err == nil {
				t.Fatal("numeric or original-association boundary was bypassed")
			}
		})
	}
}

func TestHTTPCloudDecodeReviewOwnershipAndOriginalFreshness(t *testing.T) {
	c, clock, raw := cloudDecodeReviewFixture(t)
	first, err := DecodeState(raw)
	if err != nil {
		t.Fatal(err)
	}
	second, err := DecodeState(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !math.IsInf(first.Cloud.Ranges[0], 1) || !math.IsInf(second.Cloud.Ranges[0], 1) {
		t.Fatal("missing cloud return became a finite value")
	}
	first.Cloud.Ranges[1] = 9
	first.Cloud.PublisherGID = strings.Repeat("f", 48)
	if second.Cloud.Ranges[1] != 4 || second.Cloud.PublisherGID == first.Cloud.PublisherGID {
		t.Fatal("separate decodes share mutable cloud storage")
	}
	if err := c.validateState(second); err != nil {
		t.Fatal(err)
	}
	clock.advance(250 * time.Millisecond)
	third, err := DecodeState(raw)
	if err != nil {
		t.Fatal(err)
	}
	if third.Cloud.CapturedUnixNS != second.Cloud.CapturedUnixNS || c.validateState(third) == nil {
		t.Fatal("redecoding renewed the original timestamp or exclusive age limit")
	}
}
