package twin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/twinrobot"
	"github.com/wendylabsinc/wendy/go/internal/twinvision"
	"google.golang.org/grpc"
)

var ErrRobotBusy = errors.New("robot is controlled by another session; close it or wait 15 seconds after it disconnects")

type robotSession struct {
	robot                      robotControl
	serial                     string
	lastActivity               time.Time
	pendingMu                  sync.Mutex
	pending                    twinrobot.SiteFusionReport
	pendingFrame               string
	pendingSequence            uint64
	pendingRoot                uint64
	searchMu                   sync.Mutex
	searchCancel               context.CancelFunc
	searchRoot                 uint64
	searchState, searchMessage string
	searchTurns                int
	searchReport               json.RawMessage
}
type Server struct {
	Store        *Store
	Conn         grpc.ClientConnInterface
	Context      context.Context
	mu           sync.Mutex
	openMu       sync.Mutex
	robots       map[string]*robotSession
	observers    map[string]*robotSession
	people       People
	cameraMu     sync.Mutex
	cameras      map[string]*cameraAlignment
	cameraWorker chan struct{}
	cameraSolver func(context.Context, []byte, twinvision.Frame, [9]float64, int32, bool) (twinvision.Result, error)
}

func (s *Server) Handler() http.Handler {
	s.robots = map[string]*robotSession{}
	s.observers = map[string]*robotSession{}
	s.cameras = map[string]*cameraAlignment{}
	s.cameraWorker = make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/camera-alignment", s.cameraRequest)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /v1/rooms", func(w http.ResponseWriter, r *http.Request) { v, e := s.Store.List(); respond(w, v, e) })
	mux.HandleFunc("GET /v1/rooms/{id}", func(w http.ResponseWriter, r *http.Request) { v, e := s.Store.Get(r.PathValue("id")); respond(w, v, e) })
	mux.HandleFunc("GET /v1/rooms/{id}/state", func(w http.ResponseWriter, r *http.Request) {
		v, e := s.Store.State(r.PathValue("id"))
		respond(w, v, e)
	})
	mux.HandleFunc("PUT /v1/rooms/{id}", func(w http.ResponseWriter, r *http.Request) {
		var v Room
		if !decode(w, r, &v, 190<<20) {
			return
		}
		if !strings.EqualFold(v.Site.ID, r.PathValue("id")) {
			respond(w, nil, errors.New("ID mismatch"))
			return
		}
		out, e := s.Store.Put(v)
		respond(w, out, e)
	})
	mux.HandleFunc("POST /v1/rooms/{id}/contributions", func(w http.ResponseWriter, r *http.Request) {
		var v Contribution
		if !decode(w, r, &v, 8<<20) {
			return
		}
		respond(w, struct{}{}, s.Store.Contribute(r.PathValue("id"), v))
	})
	mux.HandleFunc("POST /v1/rooms/{id}/references/import", func(w http.ResponseWriter, r *http.Request) {
		var input ReferenceImport
		if !decode(w, r, &input, 3<<20) {
			return
		}
		respond(w, struct{}{}, s.Store.ImportReferences(r.PathValue("id"), input))
	})
	mux.HandleFunc("POST /v1/rooms/{id}/people", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			ModelRevision string              `json:"modelRevision"`
			Observations  []PersonObservation `json:"observations"`
		}
		if !decode(w, r, &input, 1<<20) {
			return
		}
		room, e := s.Store.Frame(r.PathValue("id") + "/" + input.ModelRevision)
		if e != nil {
			respond(w, nil, e)
			return
		}
		result, e := s.people.Update(strings.ToLower(room.Site.ID), input.Observations, time.Now())
		respond(w, result, e)
	})
	mux.HandleFunc("POST /v1/robot", s.robotRequest)
	return mux
}
func decode(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		http.Error(w, e.Error(), 400)
		return false
	}
	if e := d.Decode(new(any)); e != io.EOF {
		http.Error(w, "expected one JSON value", 400)
		return false
	}
	return true
}
func respond(w http.ResponseWriter, v any, e error) {
	if e != nil {
		code := 400
		if errors.Is(e, ErrConflict) || errors.Is(e, ErrRobotBusy) {
			code = 409
		}
		http.Error(w, e.Error(), code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
func (s *Server) robotRequest(w http.ResponseWriter, r *http.Request) {
	var q twinrobot.Request
	if !decode(w, r, &q, 16<<20) {
		return
	}
	var out twinrobot.Response
	var err error
	// Observers read the existing stream without acquiring or replacing control.
	if q.Operation == "observe" {
		s.mu.Lock()
		session := s.observers[q.Serial]
		s.mu.Unlock()
		if session == nil {
			respond(w, nil, errors.New("robot observation is not available"))
			return
		}
		out.Snapshot = session.robot.Status()
		respond(w, out, nil)
		return
	}
	if q.Operation == "open" {
		s.openMu.Lock()
		defer s.openMu.Unlock()
		s.mu.Lock()
		previous := s.observers[q.Serial]
		for _, v := range s.robots {
			if v.serial == q.Serial && (v.lastActivity.IsZero() || time.Since(v.lastActivity) < 15*time.Second) {
				s.mu.Unlock()
				respond(w, nil, ErrRobotBusy)
				return
			}
		}
		for key, v := range s.robots {
			if v.serial == q.Serial {
				delete(s.robots, key)
			}
		}
		delete(s.observers, q.Serial)
		s.mu.Unlock()
		if previous != nil {
			previous.searchMu.Lock()
			if previous.searchCancel != nil {
				previous.searchCancel()
			}
			previous.searchMu.Unlock()
			previous.robot.Close()
		}

		var robot *twinrobot.SiteRobot
		robot, err = twinrobot.Open(s.Context, s.Conn, q.Serial)
		if err == nil {
			out.Handle = uuid.NewString()
			s.mu.Lock()
			session := &robotSession{robot: robot, serial: q.Serial, lastActivity: time.Now()}
			s.robots[out.Handle] = session
			s.observers[q.Serial] = session
			s.mu.Unlock()
		}
		respond(w, out, err)
		return
	}
	s.mu.Lock()
	session, ok := s.robots[q.Handle]
	if ok {
		session.lastActivity = time.Now()
	}
	s.mu.Unlock()
	if !ok {
		respond(w, nil, errors.New("robot session expired"))
		return
	}
	robot := session.robot

	switch q.Operation {
	case "align", "destination", "localize", "confirm":
		if _, e := s.Store.Frame(q.Frame); e != nil {
			respond(w, nil, e)
			return
		}
	}

	const stride = uint64(1_000_000)
	if q.Sequence > (^uint64(0))/stride || q.MatchSequence > (^uint64(0))/stride {
		respond(w, nil, errors.New("sequence overflow"))
		return
	}
	q.Sequence *= stride
	q.MatchSequence *= stride
	if q.Operation != "poll" && q.Operation != "video" && q.Operation != "search-poll" {
		session.searchMu.Lock()
		if session.searchCancel != nil {
			session.searchCancel()
		}
		session.searchMu.Unlock()
	}
	switch q.Operation {
	case "search":
		err = s.startSearch(session, q)
	case "search-poll":
		session.searchMu.Lock()
		out.SearchState = session.searchState
		out.SearchMessage = session.searchMessage
		out.SearchTurns = session.searchTurns
		out.Fusion = session.searchReport
		session.searchMu.Unlock()
	case "poll":
		out.Snapshot = robot.Snapshot()
	case "video":
		out.Video, err = robot.NextVideo()
	case "close":
		robot.ReleaseOperator()
		s.mu.Lock()
		delete(s.robots, q.Handle)
		s.mu.Unlock()
	case "stop":
		robot.Stop(q.Sequence)
	case "start-room":
		room := Room{Site: Site{Version: 1, ID: uuid.NewString(), Name: "Go2 " + session.serial, ModelRevision: uuid.NewString(), MetresPerModelUnit: 1, Cameras: []json.RawMessage{}}}
		err = robot.StartRoomFromOdometry(q.Sequence, room.Site.ID+"/"+room.Site.ModelRevision)
		if err == nil {
			room, err = s.Store.Put(room)
			out.RoomID = room.Site.ID
		}
	case "align":
		err = robot.Align(q.Sequence, q.Frame, q.X, q.Z, q.Floor, q.Yaw)
	case "destination":
		err = robot.Destination(q.Sequence, q.Frame, q.X, q.Z, q.Enabled)
	case "look":
		err = robot.Look(q.Sequence, q.Direction)
	case "timing":
		err = robot.SetArrivalTiming(q.Sequence, q.Enabled)
	case "underbody":
		err = robot.SetUnderBodyAllowance(q.Sequence, q.Enabled)
	case "floor":
		err = robot.SetAllowUnobservedFloor(q.Sequence, q.Enabled)
	case "localize":
		out.Candidates, err = robot.Localize(q.Sequence, q.Frame, q.Mesh)
	case "confirm":
		session.pendingMu.Lock()
		match := q.MatchSequence
		if session.pendingRoot == match {
			match = session.pendingSequence
		}
		err = robot.ConfirmLocalization(q.Sequence, match, q.Frame, q.Index)
		if err == nil && (session.pendingSequence == q.MatchSequence || session.pendingRoot == q.MatchSequence) && session.pendingFrame == q.Frame {
			err = s.Store.SaveReferences(q.Frame, session.serial, session.pending.Reference)
		}
		session.pendingMu.Unlock()
	case "fuse":
		var input twinrobot.SiteFusionRequest
		err = json.Unmarshal(q.Fusion, &input)
		if err == nil {
			var room Room
			room, err = s.Store.Frame(input.Frame)
			if err == nil {
				input.Reference = room.References[session.serial]
				var report twinrobot.SiteFusionReport
				report, err = robot.FuseLocalization(q.Sequence, input)
				if err == nil {
					out.Fusion, err = json.Marshal(report)
					session.pendingMu.Lock()
					if q.Sequence > session.pendingSequence {
						session.pending = report
						session.pendingFrame = input.Frame
						session.pendingSequence = q.Sequence
						session.pendingRoot = q.Sequence
					}
					session.pendingMu.Unlock()
				}
			}
		}
	default:
		err = errors.New("unknown robot operation")
	}
	respond(w, out, err)
}

// RunMapping keeps capturing when no renderer is connected. It must never call
// Snapshot: rendering polls alone renew the supervised movement lease.
func (s *Server) RunMapping() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.Context.Done():
			s.mu.Lock()
			for _, v := range s.observers {
				v.robot.Close()
			}
			s.mu.Unlock()
			return
		case <-ticker.C:
			rooms, e := s.Store.List()
			if e != nil {
				continue
			}
			s.mu.Lock()
			sessions := make([]*robotSession, 0, len(s.observers))
			for _, v := range s.observers {
				sessions = append(sessions, v)
			}
			s.mu.Unlock()
			for _, v := range sessions {
				frame, seq, vertices := v.robot.MappingSample()
				if len(vertices) == 0 {
					continue
				}
				for _, room := range rooms {
					// Apple frame keys include both site ID and model revision.
					if !strings.EqualFold(frame, room.Site.ID+"/"+room.Site.ModelRevision) {
						continue
					}
					points := make([]Point, len(vertices))
					for i, p := range vertices {
						points[i] = Point{p.X, p.Y, p.Z}
					}
					_ = s.Store.Contribute(room.Site.ID, Contribution{Source: "go2:" + v.serial, Sequence: seq, ModelRevision: room.Site.ModelRevision, Points: points})
				}
			}
		}
	}
}
