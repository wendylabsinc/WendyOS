package query

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

func TestParse(t *testing.T) {
	at := spatial.Stamp(1_767_258_000_000_000_000)
	for _, tc := range []struct {
		text string
		want Query
	}{
		{"find", Query{}},
		{
			"nearest 3 kind=object class=chair to rover-1 within 5m visible",
			Query{Nearest: 3, Kinds: []spatial.EntityKind{spatial.EntityObject}, Classes: []string{"chair"}, Origin: EntityRef("rover-1"), WithinM: 5, Visible: true},
		},
		{
			`find  kind=track,device   class="office chair",desk,"" on ground from device/rover-1 path 12.5`,
			Query{Kinds: []spatial.EntityKind{spatial.EntityTrack, spatial.EntityDevice}, Classes: []string{"office chair", "desk", ""}, Floor: "ground", Origin: EntityRef("device/rover-1"), PathWithinM: 12.5},
		},
		{
			"find in box -1,0,2.5,4e0 in region kitchen in polygon 0,0 1,0 1,1 limit 7",
			Query{Box: &Box{Min: geom.Vec2{X: -1}, Max: geom.Vec2{X: 2.5, Z: 4}}, Region: "kitchen", Polygon: []geom.Vec2{{}, {X: 1}, {X: 1, Z: 1}}, Limit: 7},
		},
		{
			`find near @1,2:upper 3m near "@home" 0.5 same-region rover-1 visible-from cam-lobby/camera inside region/g-lab`,
			Query{Relations: []Relation{
				{Kind: RelNear, To: *PointRef(geom.Vec2{X: 1, Z: 2}, "upper"), DistanceM: 3},
				{Kind: RelNear, To: *EntityRef("@home"), DistanceM: 0.5},
				{Kind: RelSameRegion, To: *EntityRef("rover-1")},
				{Kind: RelVisibleFrom, To: *EntityRef("cam-lobby/camera")},
				{Kind: RelInside, To: *EntityRef("region/g-lab")},
			}},
		},
		{
			"find since now-90s before now at 2026-01-01T08:59:30.5Z order time desc",
			Query{SeenSince: at.Add(-90 * time.Second), SeenBefore: at, ValidAt: at.Add(-29500 * time.Millisecond), Order: OrderTime, Descending: true},
		},
		{"find to @0.5,-2 order distance asc", Query{Origin: &Ref{Point: &geom.Vec2{X: 0.5, Z: -2}}, Order: OrderDistance}},
		{"find to x order path", Query{Origin: EntityRef("x"), Order: OrderPath}},
		{"find order key", Query{Order: OrderKey}},
	} {
		got, err := Parse(tc.text, at)
		if err != nil {
			t.Errorf("%q: %v", tc.text, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q\n got %+v\nwant %+v", tc.text, got, tc.want)
		}
		// The canonical text and the JSON form both read back to the same query.
		again, err := Parse(got.String(), time.Time{})
		if err != nil || !reflect.DeepEqual(again, got) {
			t.Errorf("%q: text round trip through %q: %+v, %v", tc.text, got.String(), again, err)
		}
		data, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		var decoded Query
		if err := json.Unmarshal(data, &decoded); err != nil || !reflect.DeepEqual(decoded, got) {
			t.Errorf("%q: JSON round trip through %s: %+v, %v", tc.text, data, decoded, err)
		}
	}
	if got := (Query{}).String(); got != "find" {
		t.Errorf("zero query prints %q", got)
	}
	if data, _ := json.Marshal(Query{}); string(data) != "{}" {
		t.Errorf("zero query marshals to %s", data)
	}
}

func TestParseErrors(t *testing.T) {
	for _, tc := range []struct {
		text  string
		pos   int
		token string
		msg   string // a fragment of the message
	}{
		{"", 0, "", `"find" or "nearest K"`},
		{"   ", 3, "", `"find" or "nearest K"`},
		{"chairs near me", 0, "chairs", "starts with"},
		{"nearest", 7, "", "count"},
		{"nearest three to x", 8, "three", "count"},
		{"nearest 0 to x", 8, "0", "count"},
		{"nearest 3 kind=object", 0, "nearest", "origin"},
		{"find kind=object within 5m", 17, "within", "origin"},
		{"find visible", 5, "visible", "origin"},
		{"find order distance", 11, "distance", "origin"},
		{"find kind=robot", 5, "kind=robot", "unknown entity kind"},
		{"find kind=", 5, "kind=", "unknown entity kind"},
		{"find class=", 5, "class=", "class=NAME"},
		{`find class="chair`, 5, `class="chair`, "unterminated quote"},
		{"find to x within 5x", 17, "5x", "distance"},
		{"find to x within -1", 17, "-1", "positive distance"},
		{"find to x within", 16, "", "distance"},
		{"find to", 7, "", "entity id or a point"},
		{"find to @1", 8, "@1", "point"},
		{"find to @1,b", 8, "@1,b", "point"},
		{"find to @1,2:", 8, "@1,2:", "floor"},
		{"find in", 7, "", `"box", "region" or "polygon"`},
		{"find in circle 1,2", 8, "circle", "unknown clause"},
		{"find in box 1,2,3", 12, "1,2,3", "box"},
		{"find in box 3,0,1,5", 12, "3,0,1,5", "x0 <= x1"},
		{"find in polygon 0,0 1,1 limit 2", 8, "polygon", "3 to 256 vertices"},
		{"find in region", 14, "", "region"},
		{"find near x", 11, "", "distance"},
		{"find since yesterday", 11, "yesterday", "time"},
		{"find since now+5s", 11, "now+5s", "time"},
		{"find at 2026-13-01T00:00:00Z", 8, "2026-13-01T00:00:00Z", "time"},
		{"find order size", 11, "size", "order"},
		{"find limit", 10, "", "limit"},
		{"find limit -3", 11, "-3", "limit"},
		{"find on", 7, "", "floor"},
		{"find on a on b", 10, "on", "duplicate on"},
		{"find to a from b", 10, "from", "duplicate to"},
		{"find kind=object kind=track", 17, "kind=track", "duplicate kind"},
		{"find in box 0,0,1,1 in box 0,0,2,2", 23, "box", "duplicate in box"},
		{"find kind=object chairs", 17, "chairs", "unknown clause"},
		{"find kind = object", 5, "kind", "unknown clause"},
	} {
		_, err := Parse(tc.text, epoch)
		var pe *ParseError
		if !errors.As(err, &pe) || !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: error %v, want a ParseError", tc.text, err)
			continue
		}
		if pe.Pos != tc.pos || pe.Token != tc.token || !strings.Contains(pe.Msg, tc.msg) {
			t.Errorf("%q: %v (pos %d token %q), want pos %d token %q message with %q", tc.text, pe, pe.Pos, pe.Token, tc.pos, tc.token, tc.msg)
		}
	}
	// A relative time needs the caller's clock.
	var pe *ParseError
	if _, err := Parse("find since now-5s", time.Time{}); !errors.As(err, &pe) || pe.Pos != 11 || !strings.Contains(pe.Msg, "current time") {
		t.Errorf("relative time without a clock: %v", err)
	}
	if got := (&ParseError{Pos: 17, Token: "5x", Msg: "expected a distance"}).Error(); got != `query: expected a distance at offset 17 near "5x"` {
		t.Errorf("message: %s", got)
	}
}

// FuzzParse: Parse never panics, and whatever it accepts prints to text that
// parses to the same query.
func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		"find", "nearest 3 kind=object class=chair to rover-1 within 5m visible",
		`find class="a b","c,d" on "f 1" to "@x" path 3 in box 0,0,1,1 in region "r r" in polygon 0,0 1,0 1,1`,
		"find near @1,2:f 3m same-region a visible-from b inside c since now-1s before now at 2026-01-01T09:00:00Z order path desc limit 4",
		`find class="\"" to @1e308,2`, "find to \"\\", "nearest 9999999999999999999 to x", "find in polygon 1,1 2,2",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		q, err := Parse(text, epoch)
		if err != nil {
			var pe *ParseError
			if !errors.As(err, &pe) || pe.Pos < 0 || pe.Pos > len(text) {
				t.Fatalf("%q: error %v is not a positioned ParseError", text, err)
			}
			return
		}
		again, err := Parse(q.String(), time.Time{})
		if err != nil || !reflect.DeepEqual(again, q) {
			t.Fatalf("%q parsed to %+v, printed as %q, read back as %+v (%v)", text, q, q.String(), again, err)
		}
	})
}
