package twin

import (
	"bytes"
	"context"
	"errors"
	"image/jpeg"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/twinrobot"
)

type objectEvidence struct {
	view    *twinrobot.ObjectView
	box     [4]float64
	created time.Time
}

type objectObserver interface {
	BeginObjectView(int64, int, int) (*twinrobot.ObjectView, error)
	LocateObjectView(*twinrobot.ObjectView, [4]float64) (twinrobot.ObjectTarget, error)
}

type robotFindRequest struct {
	CameraMode string `json:"camera_mode"`
	Serial     string `json:"serial"`
	Query      string `json:"query"`
	Model      string `json:"model,omitempty"`
	Image      []byte `json:"image"`
	Received   int64  `json:"gateway_received_unix_nanos"`
}

type robotFindResponse struct {
	Seen       bool                    `json:"seen"`
	Navigable  bool                    `json:"navigable"`
	Query      string                  `json:"query"`
	Model      string                  `json:"model"`
	Source     string                  `json:"source"`
	Box        [4]float64              `json:"box,omitempty"`
	EvidenceID string                  `json:"evidence_id,omitempty"`
	Target     *twinrobot.ObjectTarget `json:"target,omitempty"`
	Message    string                  `json:"message"`
}

// Only authenticated twin transport can reach this endpoint. Images go to the
// gateway's loopback Ollama runtime; neither callers nor models select a host.
func (s *Server) robotFindObject(w http.ResponseWriter, r *http.Request) {
	transport := &http.Transport{DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 90 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("local model redirect refused") }}
	s.robotFindObjectUsing(w, r, client)
}

func (s *Server) robotFindObjectUsing(w http.ResponseWriter, r *http.Request, client *http.Client) {
	var q robotFindRequest
	if !decode(w, r, &q, 3<<20) {
		return
	}
	q.Query = strings.TrimSpace(q.Query)
	if q.Query == "" || len(q.Query) > 160 || strings.ContainsFunc(q.Query, unicode.IsControl) || len(q.Model) > 128 {
		respond(w, nil, errors.New("describe one object with 1 to 160 printable characters"))
		return
	}
	if len(q.Image) == 0 || len(q.Image) > 2<<20 {
		respond(w, nil, errors.New("send one bounded Go2 JPEG"))
		return
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(q.Image))
	if err != nil || cfg.Width > 1280 || cfg.Height > 1280 || cfg.Width < 64 || cfg.Height < 64 {
		respond(w, nil, errors.New("invalid Go2 JPEG dimensions"))
		return
	}
	if _, err = jpeg.Decode(bytes.NewReader(q.Image)); err != nil {
		respond(w, nil, errors.New("invalid Go2 JPEG"))
		return
	}
	s.mu.Lock()
	session := s.observers[q.Serial]
	s.mu.Unlock()
	if session == nil {
		respond(w, nil, errors.New("use robot_command action connect before finding objects"))
		return
	}
	observer, ok := session.robot.(objectObserver)
	var view *twinrobot.ObjectView
	var geometryErr error
	if ok && q.CameraMode == "go2-front-native-1280x720" && cfg.Width == 1280 && cfg.Height == 720 {
		view, geometryErr = observer.BeginObjectView(q.Received, cfg.Width, cfg.Height)
	} else {
		geometryErr = errors.New("controller or camera mode does not support this object localization preset")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 100*time.Second)
	defer cancel()
	// Explicit transport has no environment proxy or redirect path to a remote
	// hosted provider. Model selection is confined to already installed models.
	model, err := selectVisionModel(ctx, client, 11434, q.Model)
	if err != nil {
		respond(w, nil, err)
		return
	}
	box, seen, err := model.locate(ctx, q.Image, q.Query)
	if err != nil {
		respond(w, nil, err)
		return
	}
	out := robotFindResponse{Seen: seen, Query: q.Query, Model: model.model, Source: "go2_onboard_camera_local_ollama", Box: box, Message: "No clear match in the current Go2 camera image. No movement was sent."}
	if !seen {
		respond(w, out, nil)
		return
	}
	out.Message = "Object is visible. "
	if geometryErr != nil {
		out.Message += geometryErr.Error() + ". No navigable target was created."
		respond(w, out, nil)
		return
	}
	target, err := observer.LocateObjectView(view, box)
	if err != nil {
		out.Message += err.Error() + ". No navigable target was created."
		respond(w, out, nil)
		return
	}
	token := uuid.NewString()
	session.objectMu.Lock()
	if session.objectEvidence == nil {
		session.objectEvidence = map[string]objectEvidence{}
	}
	for key, ev := range session.objectEvidence {
		if time.Since(ev.created) > 2*time.Minute {
			delete(session.objectEvidence, key)
		}
	}
	if len(session.objectEvidence) >= 16 {
		session.objectMu.Unlock()
		out.Message += "Object evidence cache is full; retry after two minutes."
		respond(w, out, nil)
		return
	}
	session.objectEvidence[token] = objectEvidence{view: view, box: box, created: time.Now()}
	session.objectMu.Unlock()
	out.Navigable = true
	out.EvidenceID = token
	out.Target = &target
	out.Message = "Object found in the Go2 camera with a candidate association to matching measured 3D geometry using the community camera preset (estimated calibration). A stand-off route exists on observed clear floor. No movement was sent. Use go_to_object with this evidence_id to approach; geometry is rechecked at dispatch."
	respond(w, out, nil)
}

// objectEvidenceCommand never accepts model coordinates. ExecuteCommand
// atomically revalidates this immutable evidence before claiming motion.
func objectEvidenceCommand(session *robotSession, token string) (twinrobot.Command, error) {
	session.objectMu.Lock()
	defer session.objectMu.Unlock()
	ev, ok := session.objectEvidence[token]
	if !ok || time.Since(ev.created) > 2*time.Minute {
		return twinrobot.Command{}, errors.New("object evidence is missing or expired; use robot_find_object again")
	}
	return twinrobot.Command{Action: "approach_object", ObjectView: ev.view, ObjectBox: ev.box}, nil
}
