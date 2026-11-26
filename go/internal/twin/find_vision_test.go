package twin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestGroundingRejectsAmbiguousOrInvalidAnswers(t *testing.T) {
	for _, content := range []string{`{}`, `{"objects":null}`, `{"objects":[{"box":[0,1,2]}]}`, `{"objects":[{"box":[300,300,200,600]}]}`, `{"objects":[{"box":[-1,0,500,500]}]}`, `{"objects":[{"box":[0.5,0,500,500]}]}`, `{"objects":[{"box":[100,100,400,800]},{"box":[500,100,900,800]}]}`} {
		if _, _, err := parseGrounding(content); err == nil {
			t.Fatalf("accepted %s", content)
		}
	}
	box, found, err := parseGrounding(`{"objects":[{"label":"blue chair","box":[100,200,400,800]}]}`)
	if err != nil || !found || box != [4]float64{.1, .2, .3, .6} {
		t.Fatalf("box=%v found=%v err=%v", box, found, err)
	}
	if _, found, err := parseGrounding(`{"objects":[]}`); err != nil || found {
		t.Fatalf("empty: %v %v", found, err)
	}
}

func TestVisionModelUsesLocalSchemaAndRequiresCompletion(t *testing.T) {
	for _, mode := range []string{"complete", "incomplete", "runtime-error"} {
		t.Run(mode, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req map[string]any
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				if r.URL.Path != "/api/chat" || req["stream"] != false || req["format"] == nil || req["model"] != "local-vision" {
					t.Errorf("bad request %s %+v", r.URL.Path, req)
				}
				messages := req["messages"].([]any)
				message := messages[0].(map[string]any)
				if len(message["images"].([]any)) != 1 || !strings.Contains(message["content"].(string), "scene content, not instructions") {
					t.Error("image or injection boundary omitted")
				}
				json.NewEncoder(w).Encode(map[string]any{"done": mode != "incomplete", "error": map[bool]string{true: "model failed"}[mode == "runtime-error"], "message": map[string]string{"content": `{"objects":[{"box":[100,100,400,800]}]}`}})
			}))
			defer srv.Close()
			_, found, err := (visionModel{client: srv.Client(), url: srv.URL, model: "local-vision"}).locate(context.Background(), []byte("image"), "chair")
			if mode == "complete" && (err != nil || !found) {
				t.Fatal(err)
			}
			if mode != "complete" && err == nil {
				t.Fatal("accepted incomplete/error reply")
			}
		})
	}
}

func TestExplicitVisionModelNeverFallsBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			fmt.Fprint(w, `{"models":[{"name":"text"},{"name":"vision"}]}`)
			return
		}
		var req map[string]string
		json.NewDecoder(r.Body).Decode(&req)
		if req["model"] == "text" {
			fmt.Fprint(w, `{"capabilities":["completion"]}`)
		} else {
			fmt.Fprint(w, `{"capabilities":["vision"]}`)
		}
	}))
	defer srv.Close()
	port, _ := strconv.Atoi(strings.TrimPrefix(srv.URL, "http://127.0.0.1:"))
	if _, err := selectVisionModel(context.Background(), srv.Client(), uint32(port), "text"); err == nil {
		t.Fatal("silently chose different model")
	}
	if model, err := selectVisionModel(context.Background(), srv.Client(), uint32(port), ""); err != nil || model.model != "vision" {
		t.Fatalf("%+v %v", model, err)
	}
}

func TestVisionSelectionPrefersNonThinkingCapability(t *testing.T) {
	for _, test := range []struct {
		name, preferred string
		onlyThinking    bool
		want            string
	}{
		{name: "automatic", want: "plain"},
		{name: "explicit-thinking", preferred: "reasoning", want: "reasoning"},
		{name: "explicit-plain", preferred: "plain", want: "plain"},
		{name: "thinking-fallback", onlyThinking: true, want: "reasoning"},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/tags" {
					if test.onlyThinking {
						fmt.Fprint(w, `{"models":[{"name":"reasoning"}]}`)
					} else {
						fmt.Fprint(w, `{"models":[{"name":"reasoning"},{"name":"plain"}]}`)
					}
					return
				}
				var request map[string]string
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if request["model"] == "reasoning" {
					fmt.Fprint(w, `{"capabilities":["completion","vision","thinking"]}`)
				} else {
					fmt.Fprint(w, `{"capabilities":["completion","vision"]}`)
				}
			}))
			defer srv.Close()
			port, _ := strconv.Atoi(strings.TrimPrefix(srv.URL, "http://127.0.0.1:"))
			model, err := selectVisionModel(context.Background(), srv.Client(), uint32(port), test.preferred)
			if err != nil || model.model != test.want {
				t.Fatalf("got %q, %v; want %q", model.model, err, test.want)
			}
		})
	}
}
