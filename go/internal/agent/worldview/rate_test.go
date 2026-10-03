package worldview

import (
	"sync"
	"testing"
	"time"
)

func TestRateMeter(t *testing.T) {
	// steady ticks fps frames per second from t0 through untilMs inclusive.
	steady := func(r *RateMeter, untilMs int, fps int) {
		for i := 0; ; i++ {
			d := time.Duration(i) * time.Second / time.Duration(fps)
			if d > time.Duration(untilMs)*time.Millisecond {
				return
			}
			r.Tick(t0.Add(d))
		}
	}
	cases := []struct {
		name  string
		setup func(*RateMeter)
		now   int
		want  float64
	}{
		{"no ticks", func(*RateMeter) {}, 1000, 0},
		{"one tick", func(r *RateMeter) { r.Tick(at(0)) }, 0, 0},
		{"one tick later", func(r *RateMeter) { r.Tick(at(0)) }, 3000, 0},
		{"warm up at 10 fps", func(r *RateMeter) { steady(r, 1000, 10) }, 1000, 10},
		{"steady 10 fps", func(r *RateMeter) { steady(r, 10000, 10) }, 10000, 10},
		{"steady 30 fps", func(r *RateMeter) { steady(r, 10000, 30) }, 10000, 30},
		{"stream stopped halfway out of window", func(r *RateMeter) { steady(r, 10000, 10) }, 12500, 5},
		{"stream stopped long ago", func(r *RateMeter) { steady(r, 10000, 10) }, 20000, 0},
		{"warm up after stop decays", func(r *RateMeter) { steady(r, 1000, 10) }, 2000, 5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var r RateMeter
			c.setup(&r)
			if got := r.Achieved(at(c.now)); !near(got, c.want) {
				t.Fatalf("Achieved = %v, want %v", got, c.want)
			}
		})
	}
}

func TestRateMeterConcurrent(t *testing.T) {
	var r RateMeter
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				now := at(i * 10)
				if g%2 == 0 {
					r.Tick(now)
				} else {
					_ = r.Achieved(now)
				}
			}
		}(g)
	}
	wg.Wait()
	if got := r.Achieved(at(5000)); got <= 0 {
		t.Fatalf("Achieved = %v after concurrent ticks", got)
	}
}
