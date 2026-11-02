package tui

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// BuildKit's `--progress=rawjson` output: one JSON object per line on stderr,
// each carrying any mix of vertex lifecycle records, byte counters, and log
// frames. It is strictly better than scraping the plain-text renderer — the byte
// counters are exact rather than re-parsed from formatted strings, and vertices
// are identified by digest instead of a display-order integer. Tool output
// inside a RUN still arrives only as raw bytes, so the sniffers do that half.
type rawJSONFrame struct {
	Vertexes []rawJSONVertex `json:"vertexes"`
	Statuses []rawJSONStatus `json:"statuses"`
	Logs     []rawJSONLog    `json:"logs"`
}

type rawJSONVertex struct {
	Digest    string     `json:"digest"`
	Name      string     `json:"name"`
	Started   *time.Time `json:"started"`
	Completed *time.Time `json:"completed"`
	Cached    bool       `json:"cached"`
	Error     string     `json:"error"`
}

type rawJSONStatus struct {
	ID        string     `json:"id"`
	Vertex    string     `json:"vertex"`
	Name      string     `json:"name"`
	Current   int64      `json:"current"`
	Total     int64      `json:"total"`
	Timestamp time.Time  `json:"timestamp"`
	Started   *time.Time `json:"started"`
	Completed *time.Time `json:"completed"`
}

type rawJSONLog struct {
	Vertex    string    `json:"vertex"`
	Stream    int       `json:"stream"`
	Data      []byte    `json:"data"` // base64 in the wire format
	Timestamp time.Time `json:"timestamp"`
}

// rawStatusCounter tracks one sub-transfer of a vertex (a layer download, an
// extract). A vertex's progress is the sum over its counters.
type rawStatusCounter struct {
	current int64
	total   int64
	started time.Time
	latest  time.Time
}

// parseRawJSONFrame decodes one rawjson line. It reports false when the line is
// not a rawjson frame so the caller can fall back to plain-text parsing.
func (p *BuildParser) parseRawJSONFrame(line string) bool {
	dec := json.NewDecoder(strings.NewReader(line))
	dec.DisallowUnknownFields()
	var f rawJSONFrame
	if err := dec.Decode(&f); err != nil {
		// Unknown fields mean a newer buildx; retry leniently before giving up.
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			return false
		}
	}
	if len(f.Vertexes) == 0 && len(f.Statuses) == 0 && len(f.Logs) == 0 {
		return false
	}
	for _, v := range f.Vertexes {
		p.onRawVertex(v)
	}
	for _, s := range f.Statuses {
		p.onRawStatus(s)
	}
	for _, l := range f.Logs {
		p.onRawLog(l)
	}
	return true
}

func (p *BuildParser) onRawVertex(rv rawJSONVertex) {
	if rv.Digest == "" {
		return
	}
	v := p.state(rv.Digest)
	if !v.named && rv.Name != "" {
		v.named = true
		v.kind, v.display = classifyBuildVertex(rv.Name)
		if rv.Started != nil {
			v.started = *rv.Started
		}
		p.emitStart(rv.Digest, v)
	}
	v.recordRun(rv.Started, rv.Completed)
	if v.terminal {
		return
	}
	switch {
	case rv.Error != "":
		p.emitStatus(rv.Digest, v, BuildStepFailed, 0)
	case rv.Cached:
		p.emitStatus(rv.Digest, v, BuildStepCached, 0)
	case rv.Completed != nil:
		p.emitStatus(rv.Digest, v, BuildStepDone, v.runDuration())
	}
}

// vertexRun is one run of a vertex; stop is zero while it is still running.
type vertexRun struct{ start, stop time.Time }

// recordRun notes one vertex record. BuildKit reports a vertex once per run,
// keyed by its start time, and a vertex can run several times: a lazily pulled
// base image resolves, finishes, then runs again for each stretch of layer
// pulling and unpacking.
func (v *buildVertexState) recordRun(started, completed *time.Time) {
	if completed != nil && completed.After(v.completedAt) {
		v.completedAt = *completed
	}
	if started == nil {
		return
	}
	if v.runs == nil {
		v.runs = map[int64]vertexRun{}
	}
	r := vertexRun{start: *started}
	if completed != nil {
		r.stop = *completed
	}
	v.runs[started.UnixNano()] = r
}

// runDuration is the time spent in finished runs, overlaps counted once: the
// figure BuildKit's own plain printer puts on its DONE line.
func (v *buildVertexState) runDuration() time.Duration {
	runs := make([]vertexRun, 0, len(v.runs))
	for _, r := range v.runs {
		if !r.stop.IsZero() {
			runs = append(runs, r)
		}
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].start.Before(runs[j].start) })
	var total time.Duration
	for i := 0; i < len(runs); {
		cur := runs[i]
		for i++; i < len(runs) && !runs[i].start.After(cur.stop); i++ {
			if runs[i].stop.After(cur.stop) {
				cur.stop = runs[i].stop
			}
		}
		total += cur.stop.Sub(cur.start)
	}
	return total
}

func (p *BuildParser) onRawStatus(rs rawJSONStatus) {
	if rs.Vertex == "" {
		return
	}
	v := p.state(rs.Vertex)
	if !v.named || v.kind == BuildVertexHidden {
		return
	}
	// Still in progress and written after the vertex last finished: new work,
	// not the tail of the finished run arriving a frame late.
	open := rs.Completed == nil && (v.completedAt.IsZero() || rs.Timestamp.After(v.completedAt))
	if strings.HasPrefix(rs.ID, "extracting ") {
		p.onExtract(rs.Vertex, v, rs.ID, rs.Completed != nil, open)
		return
	}
	if v.statuses == nil {
		v.statuses = map[string]*rawStatusCounter{}
	}
	c := v.statuses[rs.ID]
	if c == nil {
		c = &rawStatusCounter{}
		v.statuses[rs.ID] = c
	}
	c.current, c.total = rs.Current, rs.Total
	if rs.Started != nil && c.started.IsZero() {
		c.started = *rs.Started
	}
	if rs.Timestamp.After(c.latest) {
		c.latest = rs.Timestamp
	}

	// Sum every sub-transfer so a multi-layer pull reads as one figure. Ignore
	// counters with no declared total: BuildKit reports "done" markers that way
	// and mixing them in would make the percentage jump around.
	var bp ByteProgress
	var earliest, newest time.Time
	for _, sc := range v.statuses {
		if sc.total <= 0 {
			continue
		}
		bp.Current += sc.current
		bp.Total += sc.total
		if earliest.IsZero() || (!sc.started.IsZero() && sc.started.Before(earliest)) {
			earliest = sc.started
		}
		if sc.latest.After(newest) {
			newest = sc.latest
		}
	}
	if bp.Empty() {
		return
	}
	if !earliest.IsZero() && newest.After(earliest) {
		if secs := newest.Sub(earliest).Seconds(); secs > 0 {
			bp.Rate = float64(bp.Current) / secs
		}
	}
	// Only a sized transfer reopens a finished vertex: BuildKit sends unsized
	// bookkeeping ("transferring dockerfile: 135B") after a setup step has
	// already reported done, and that is not worth a second row.
	p.onBytes(rs.Vertex, v, bp, open && rs.Total > 0)
}

func (p *BuildParser) onRawLog(rl rawJSONLog) {
	if rl.Vertex == "" || len(rl.Data) == 0 {
		return
	}
	v := p.state(rl.Vertex)
	if !v.named || v.kind == BuildVertexHidden || v.terminal {
		return
	}
	// Log frames are byte chunks, not lines: buffer the tail and split on both
	// \n and \r so carriage-return progress meters (curl, wget, pip) advance.
	v.logTail = append(v.logTail, rl.Data...)
	for {
		i := bytes.IndexAny(v.logTail, "\n\r")
		if i < 0 {
			break
		}
		line := string(v.logTail[:i])
		v.logTail = v.logTail[i+1:]
		p.onLogLine(rl.Vertex, v, line)
	}
	// Guard against a tool that never emits a line break.
	if len(v.logTail) > 8<<10 {
		p.onLogLine(rl.Vertex, v, string(v.logTail))
		v.logTail = v.logTail[:0]
	}
}
