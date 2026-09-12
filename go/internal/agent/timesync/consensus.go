package timesync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/roughtime"
)

const maxCaptureRTT = 5 * time.Second

// RoughtimeEvidence is sufficient to independently reverify one observation.
// Offsets are UTC-CLOCK_BOOTTIME; no symmetric network delay is assumed.
type RoughtimeEvidence struct {
	Server            string `json:"server"`
	Address           string `json:"address"`
	KeyID             string `json:"key_id"`
	Nonce             []byte `json:"nonce"`
	RawResponse       []byte `json:"raw_response"`
	SendBootNanos     int64  `json:"send_boottime_nanos"`
	ReceiveBootNanos  int64  `json:"receive_boottime_nanos"`
	MidpointUnixNanos int64  `json:"midpoint_unix_nanos"`
	RadiusNanos       int64  `json:"radius_nanos"`
	LowerOffsetNanos  int64  `json:"lower_offset_nanos"`
	UpperOffsetNanos  int64  `json:"upper_offset_nanos"`
	RTTNanos          int64  `json:"rtt_nanos"`
	Included          bool   `json:"included"`
	Error             string `json:"error,omitempty"`
}

type Consensus struct {
	Confidence        string              `json:"confidence"`
	LowerOffsetNanos  int64               `json:"lower_offset_nanos,omitempty"`
	UpperOffsetNanos  int64               `json:"upper_offset_nanos,omitempty"`
	Quorum            int                 `json:"quorum"`
	Evidence          []RoughtimeEvidence `json:"evidence"`
	ObservedUnixNanos int64               `json:"observed_unix_nanos"`
}

type queryServerFunc func(context.Context, roughtime.Server) (roughtime.Result, error)

func QueryConsensus(ctx context.Context, servers []roughtime.Server) (Consensus, error) {
	return queryConsensus(ctx, servers, roughtime.QueryServer, bootTimeNanos)
}

func queryConsensus(ctx context.Context, servers []roughtime.Server, query queryServerFunc, boot func() (int64, error)) (Consensus, error) {
	evidence := make([]RoughtimeEvidence, len(servers))
	var wg sync.WaitGroup
	for i, srv := range servers {
		wg.Add(1)
		go func(i int, s roughtime.Server) {
			defer wg.Done()
			ev := RoughtimeEvidence{Server: s.Name, Address: s.Address}
			kh := sha256.Sum256(s.PublicKey)
			ev.KeyID = hex.EncodeToString(kh[:])
			send, e := boot()
			if e != nil {
				ev.Error = e.Error()
				evidence[i] = ev
				return
			}
			ev.SendBootNanos = send
			r, e := query(ctx, s)
			recv, be := boot()
			ev.ReceiveBootNanos = recv
			ev.RTTNanos = recv - send
			if e != nil {
				ev.Error = e.Error()
			} else if be != nil {
				ev.Error = be.Error()
			} else if ev.RTTNanos < 0 || ev.RTTNanos > maxCaptureRTT.Nanoseconds() {
				ev.Error = "implausible RTT"
			} else {
				ev.Nonce = r.Nonce
				ev.RawResponse = r.RawResponse
				ev.MidpointUnixNanos = r.Midpoint.UnixNano()
				ev.RadiusNanos = r.Radius.Nanoseconds()
				ev.LowerOffsetNanos = ev.MidpointUnixNanos - ev.RadiusNanos - recv
				ev.UpperOffsetNanos = ev.MidpointUnixNanos + ev.RadiusNanos - send
			}
			evidence[i] = ev
		}(i, srv)
	}
	wg.Wait()
	c := Consensus{Confidence: "unbounded", Evidence: evidence, ObservedUnixNanos: time.Now().UnixNano()}
	idx, lo, hi := largestIntersection(evidence)
	for _, i := range idx {
		c.Evidence[i].Included = true
	}
	c.Quorum = len(idx)
	if len(idx) >= 3 {
		c.Confidence = "verified"
		c.LowerOffsetNanos = lo
		c.UpperOffsetNanos = hi
	} else if len(idx) == 2 {
		c.Confidence = "degraded"
		c.LowerOffsetNanos = lo
		c.UpperOffsetNanos = hi
	}
	if c.Quorum == 0 {
		var errs []string
		for _, e := range evidence {
			if e.Error != "" {
				errs = append(errs, e.Server+": "+e.Error)
			}
		}
		return c, fmt.Errorf("no valid Roughtime evidence: %s", strings.Join(errs, "; "))
	}
	return c, nil
}

func largestIntersection(ev []RoughtimeEvidence) ([]int, int64, int64) {
	type endpoint struct {
		lower int64
		index int
	}
	var starts []endpoint
	for i, e := range ev {
		if e.Error == "" && e.UpperOffsetNanos >= e.LowerOffsetNanos {
			starts = append(starts, endpoint{e.LowerOffsetNanos, i})
		}
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i].lower < starts[j].lower })
	var best []int
	var bestLo, bestHi int64
	for _, start := range starts {
		var set []int
		hi := int64(^uint64(0) >> 1)
		for i, e := range ev {
			if e.Error == "" && e.LowerOffsetNanos <= start.lower && e.UpperOffsetNanos >= start.lower {
				set = append(set, i)
				if e.UpperOffsetNanos < hi {
					hi = e.UpperOffsetNanos
				}
			}
		}
		if len(set) > len(best) {
			best = set
			bestLo = start.lower
			bestHi = hi
		}
	}
	return best, bestLo, bestHi
}
