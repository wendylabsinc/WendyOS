package twin

import (
	"bytes"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot"
)

type objectEvidenceTestRobot struct {
	commandRobot
	geometryErr error
}

func (r *objectEvidenceTestRobot) BeginObjectView(int64, int, int) (*twinrobot.ObjectView, error) {
	return &twinrobot.ObjectView{}, r.geometryErr
}
func (r *objectEvidenceTestRobot) LocateObjectView(*twinrobot.ObjectView, [4]float64) (twinrobot.ObjectTarget, error) {
	return twinrobot.ObjectTarget{Frame: "room/1", ApproachX: 1, Support: 3, Calibration: "community-estimated"}, r.geometryErr
}

type findRoundTrip func(*http.Request) (*http.Response, error)

func (f findRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFindEndpointLocalCameraToEvidenceWithoutMotion(t *testing.T) {
	var jpegBytes bytes.Buffer
	jpeg.Encode(&jpegBytes, image.NewRGBA(image.Rect(0, 0, 1280, 720)), nil)
	for _, mode := range []string{"grounded", "visible-only", "no-match", "unknown-mode", "model-error", "no-observer-seen", "no-observer-absent"} {
		t.Run(mode, func(t *testing.T) {
			robot := &objectEvidenceTestRobot{}
			if mode == "visible-only" {
				robot.geometryErr = errors.New("waiting for localization")
			}
			session := &robotSession{robot: robot}
			s := &Server{observers: map[string]*robotSession{"robot": session}}
			noObserver := strings.HasPrefix(mode, "no-observer-")
			if noObserver {
				s.observers = nil
			}
			absent := mode == "no-match" || mode == "no-observer-absent"
			calls := 0
			client := &http.Client{Transport: findRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Host != "127.0.0.1:11434" {
					t.Fatal("escaped local runtime")
				}
				text := `{"models":[{"name":"vision"}]}`
				switch r.URL.Path {
				case "/api/show":
					text = `{"capabilities":["vision"]}`
				case "/api/chat":
					var body struct {
						Messages []struct {
							Images [][]byte `json:"images"`
						} `json:"messages"`
					}
					json.NewDecoder(r.Body).Decode(&body)
					if len(body.Messages) != 1 || len(body.Messages[0].Images) != 1 || !bytes.Equal(body.Messages[0].Images[0], jpegBytes.Bytes()) {
						t.Fatal("wrong camera image")
					}
					content := `{"objects":[{"box":[100,100,500,800]}]}`
					if absent {
						content = `{"objects":[]}`
					}
					output := map[string]any{"done": true, "message": map[string]string{"content": content}}
					if mode == "model-error" {
						output["error"] = "failed"
					}
					raw, _ := json.Marshal(output)
					text = string(raw)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(text))}, nil
			})}
			q := robotFindRequest{Serial: "robot", Query: "blue chair", Image: jpegBytes.Bytes(), Received: time.Now().UnixNano(), CameraMode: "go2-front-native-1280x720"}
			if mode == "unknown-mode" {
				q.CameraMode = ""
			}
			raw, _ := json.Marshal(q)
			w := httptest.NewRecorder()
			s.robotFindObjectUsing(w, httptest.NewRequest(http.MethodPost, "/v1/robot/find-object", bytes.NewReader(raw)), client)
			if len(robot.commands) != 0 {
				t.Fatal("finding object moved robot")
			}
			if mode == "model-error" {
				if w.Code == 200 {
					t.Fatal("ignored runtime error")
				}
				return
			}
			if w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			var result robotFindResponse
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Seen == absent || result.Navigable != (mode == "grounded") || calls != 3 {
				t.Fatalf("%+v calls%d", result, calls)
			}
			if mode == "grounded" {
				cmd, err := objectEvidenceCommand(session, result.EvidenceID)
				if err != nil || cmd.Action != "approach_object" || cmd.ObjectView == nil {
					t.Fatalf("%+v %v", cmd, err)
				}
			} else if result.EvidenceID != "" || result.Target != nil {
				t.Fatal("created unsafe evidence")
			}
			if noObserver {
				if len(s.observers) != 0 {
					t.Fatal("visual finding created an observer")
				}
				if !absent && !strings.Contains(result.Message, "no geometric controller") {
					t.Fatal(result.Message)
				}
			}
		})
	}
}
