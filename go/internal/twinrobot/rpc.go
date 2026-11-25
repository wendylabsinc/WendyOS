package twinrobot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
	"io"
	"net/http"
)

// Request carries intent. Planning, freshness checks and movement stay on Linux.
type Request struct {
	Operation               string
	Handle                  string
	Serial                  string
	Sequence, MatchSequence uint64
	Frame                   string
	X, Z, Floor, Yaw        float64
	Enabled                 bool
	Direction, Index        int
	Mesh                    []sitenav.Triangle
	Fusion                  json.RawMessage
	// find: what to look for, whether to bark, and the gateway's loopback
	// Ollama port and preferred vision model. find-view: one JPEG camera frame
	// answering FindStatus.WantsView.
	Query     string
	Bark      bool
	Model     string
	ModelPort uint32
	View      uint64
	Image     []byte
}

// FindStatus is the progress of one "find this object" mission. Linux owns the
// search, perception and movement; the client renders this and supplies the
// decoded camera frames Linux asks for.
type FindStatus struct {
	State     string // "", searching, found, not-found, failed
	Phase     string // recalling, looking, thinking, turning, moving, approaching, barking
	Message   string
	Query     string
	WantsView uint64 // nonzero: upload a camera frame captured after this request
	// Viewpoints counts places searched; Views counts camera frames examined.
	Viewpoints, Views int
	Remembered        bool // started at a location saved by an earlier find
	Seen, Located     bool // seen by the camera; placed in the room by LiDAR or floor geometry
	X, Z              float64
	Box               [4]float64 // x, y, width, height as fractions of frame BoxView
	BoxView           uint64
	Barked            bool
	BarkFailure       string
}
type Response struct {
	Find          FindStatus
	SearchState   string
	SearchMessage string
	SearchTurns   int
	SearchMoves   int
	SearchPhase   string
	RoomID        string
	Handle        string
	Snapshot      SiteRobotSnapshot
	Candidates    []SiteRobotCandidate
	Fusion        json.RawMessage
	Video         []byte
}
type Remote struct {
	Client      *http.Client
	URL, Handle string
}

func (r *Remote) Call(q Request) (Response, error) {
	q.Handle = r.Handle
	var result Response
	body, err := json.Marshal(q)
	if err != nil {
		return result, err
	}
	req, err := http.NewRequest("POST", r.URL+"v1/robot", bytes.NewReader(body))
	if err != nil {
		return result, err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := r.Client.Do(req)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return result, fmt.Errorf("digital twin: %s", message)
	}
	err = json.NewDecoder(io.LimitReader(response.Body, 32<<20)).Decode(&result)
	return result, err
}
func (r *Remote) Snapshot() SiteRobotSnapshot {
	v, e := r.Call(Request{Operation: "poll"})
	if e != nil {
		return SiteRobotSnapshot{Disconnected: true, Message: e.Error()}
	}
	return v.Snapshot
}
func (r *Remote) NextVideo() ([]byte, error) {
	v, e := r.Call(Request{Operation: "video"})
	return v.Video, e
}
func (r *Remote) Close()        { r.Call(Request{Operation: "close"}) }
func (r *Remote) Stop(s uint64) { r.Call(Request{Operation: "stop", Sequence: s}) }
func (r *Remote) Align(s uint64, f string, x, z, y, yaw float64) error {
	_, e := r.Call(Request{Operation: "align", Sequence: s, Frame: f, X: x, Z: z, Floor: y, Yaw: yaw})
	return e
}
func (r *Remote) Destination(s uint64, f string, x, z float64, m bool) error {
	_, e := r.Call(Request{Operation: "destination", Sequence: s, Frame: f, X: x, Z: z, Enabled: m})
	return e
}
func (r *Remote) Look(s uint64, d int) error {
	_, e := r.Call(Request{Operation: "look", Sequence: s, Direction: d})
	return e
}
func (r *Remote) SetArrivalTiming(s uint64, b bool) error {
	_, e := r.Call(Request{Operation: "timing", Sequence: s, Enabled: b})
	return e
}
func (r *Remote) SetUnderBodyAllowance(s uint64, b bool) error {
	_, e := r.Call(Request{Operation: "underbody", Sequence: s, Enabled: b})
	return e
}
func (r *Remote) SetAllowUnobservedFloor(s uint64, b bool) error {
	_, e := r.Call(Request{Operation: "floor", Sequence: s, Enabled: b})
	return e
}
func (r *Remote) Localize(s uint64, f string, m []sitenav.Triangle) ([]SiteRobotCandidate, error) {
	v, e := r.Call(Request{Operation: "localize", Sequence: s, Frame: f, Mesh: m})
	return v.Candidates, e
}
func (r *Remote) ConfirmLocalization(s, m uint64, f string, i int) error {
	_, e := r.Call(Request{Operation: "confirm", Sequence: s, MatchSequence: m, Frame: f, Index: i})
	return e
}
func (r *Remote) FuseLocalizationJSON(s uint64, b []byte) ([]byte, error) {
	v, e := r.Call(Request{Operation: "fuse", Sequence: s, Fusion: b})
	return v.Fusion, e
}

func (r *Remote) RequestJSON(data []byte) ([]byte, error) {
	var q Request
	if e := json.Unmarshal(data, &q); e != nil {
		return nil, e
	}
	reply, e := r.Call(q)
	if e != nil {
		return nil, e
	}
	return json.Marshal(reply)
}
