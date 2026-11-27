package query

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

func TestLimitedQueriesPreserveFullOrderingAndCounts(t *testing.T) {
	w := World{}
	for i := 199; i >= 0; i-- {
		floor := spatial.FloorID("a")
		if i%3 == 0 {
			floor = "b"
		}
		e := row(spatial.EntityObject, fmt.Sprintf("object-%03d", i), floor, float64(i%10), float64(i%7))
		e.ObservedAt = epoch.Add(time.Duration(i%5) * time.Second)
		w.Entities = append(w.Entities, e)
	}
	ix, err := New(w, Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, order := range []Order{OrderDefault, OrderKey, OrderTime, OrderDistance} {
		for _, descending := range []bool{false, true} {
			if order == OrderDefault && descending {
				continue
			}
			for _, nearest := range []int{0, 7, 150} {
				q := Query{Order: order, Descending: descending, Nearest: nearest}
				if order == OrderDistance || nearest > 0 {
					q.Origin = PointRef(geom.Vec2{X: 3, Z: 2}, "a")
				}
				full, err := ix.Run(q)
				if err != nil {
					t.Fatal(err)
				}
				for _, limit := range []int{1, 7, 20, 200, 1000} {
					q.Limit = limit
					got, err := ix.Run(q)
					if err != nil || got.Total != full.Total || got.Examined != full.Examined || !reflect.DeepEqual(got.Matches, full.Matches[:min(limit, len(full.Matches))]) {
						t.Fatalf("limited query changed ordering, total or examined count: %+v, err=%v", q, err)
					}
				}
			}
		}
	}
}
