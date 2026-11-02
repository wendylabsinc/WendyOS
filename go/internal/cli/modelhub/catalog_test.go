package modelhub

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPublicCatalogFilesAndSafeModelReferences(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("catalog leaked credentials")
		}
		if r.URL.Query().Get("blobs") != "true" {
			t.Error("missing file sizes")
		}
		fmt.Fprint(w, `{"id":"Qwen/test","sha":"abc","gated":false,"tags":["license:apache-2.0"],"siblings":[{"rfilename":"weights-Q8_0.gguf","size":800},{"rfilename":"weights-Q4_K_M.gguf","size":400},{"rfilename":"mmproj-F16.gguf","size":100},{"rfilename":"weights-00001-of-00002.gguf","size":200},{"rfilename":"../bad.gguf","size":1}]}`)
	}))
	defer server.Close()
	c := &Client{HTTP: server.Client(), BaseURL: server.URL}
	d, err := c.Details(context.Background(), "Qwen/test")
	if err != nil || len(d.Files) != 2 || d.Files[0].Quantization != "Q4_K_M" || d.License != "apache-2.0" {
		t.Fatalf("details: %+v %v", d, err)
	}
	name, err := ModelName("Qwen/test", d.Files[0].Name)
	if err != nil || name != "hf.co/Qwen/test:weights-Q4_K_M.gguf" {
		t.Fatal(name, err)
	}
	for _, repo := range []string{"../x", "https://bad/x", "a/b/c", "a/b?token=x", "a/.."} {
		if _, err := ModelName(repo, "model.gguf"); err == nil {
			t.Fatalf("accepted repo %q", repo)
		}
	}
}
func TestCatalogSearchEncodesQueryAndGatedModelsRefused(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models" {
			if r.URL.Query().Get("search") != "qwen & nvidia" || r.URL.Query().Get("filter") != "gguf" {
				t.Fatal(r.URL)
			}
			fmt.Fprint(w, `[{"id":"Qwen/test"}]`)
			return
		}
		fmt.Fprint(w, `{"gated":"auto","siblings":[{"rfilename":"model.gguf","size":100}]}`)
	}))
	defer server.Close()
	c := &Client{HTTP: server.Client(), BaseURL: server.URL}
	if r, err := c.Search(context.Background(), "qwen & nvidia"); err != nil || len(r) != 1 {
		t.Fatal(r, err)
	}
	if _, err := c.Details(context.Background(), "Qwen/test"); err == nil {
		t.Fatal("accepted gated model")
	}
}
func TestVisionUsesPinnedConfigAndModelVocabulary(t *testing.T) {
	const revision = "1234567890123456789012345678901234567890"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/google/test":
			fmt.Fprintf(w, `{"pipeline_tag":"image-classification","sha":%q,"tags":["license:apache-2.0"],"siblings":[{"rfilename":"model.safetensors","size":1234}]}`, revision)
		case "/google/test/resolve/" + revision + "/config.json":
			fmt.Fprint(w, `{"model_type":"vit","id2label":{"0":"cat, tabby","1":"dog","2":"N/A"}}`)
		default:
			t.Error("un-pinned or unexpected request", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	c := &Client{HTTP: server.Client(), BaseURL: server.URL}
	v, err := c.VisionDetails(context.Background(), "google/test", "image-classification")
	if err != nil || v.Revision != revision || len(v.Labels) != 2 || v.Labels[0] != "cat, tabby" || v.WeightsBytes != 1234 {
		t.Fatal(v, err)
	}
	if _, err := c.VisionDetails(context.Background(), "google/test", "object-detection"); err == nil || !strings.Contains(err.Error(), "task") {
		t.Fatal("accepted mismatched task", err)
	}
}
func TestHardwareFitUsesAvailableCapacityAndDoesNotSumGPUs(t *testing.T) {
	h := Hardware{MemoryTotal: 16 * GiB, MemoryAvailable: 12 * GiB, DiskKnown: true, DiskFree: 20 * GiB}
	if h.Fit(GiB, 0, true) != 1 {
		t.Fatal("small model should fit")
	}
	h.MemoryAvailable = 3 * GiB
	if h.Fit(GiB, 0, true) != 3 {
		t.Fatal("must use available, not total RAM")
	}
	h.MemoryAvailable = 6 * GiB
	if h.Fit(GiB, 0, true) != 2 {
		t.Fatal("missing close-to-limit warning")
	}
	h.MemoryAvailable = 12 * GiB
	if h.Fit(GiB, 1, true) != 0 {
		t.Fatal("missing GPU memory must be unknown")
	}
	h.GPUTotal = 2 * GiB
	h.GPUAvailable = GiB
	if h.Fit(GiB, 1, true) != 3 {
		t.Fatal("system RAM must not stand in for discrete GPU memory")
	}
	h.SharedMemory = true
	if h.Fit(GiB, 1, true) != 1 {
		t.Fatal("Jetson should use shared memory budget")
	}
	h.DiskFree = 6 * GiB
	if h.Fit(GiB, 0, true) != 3 {
		t.Fatal("runtime storage headroom missing")
	}
	h.DiskKnown = false
	if h.Fit(GiB, 0, true) != 0 {
		t.Fatal("unknown storage is not a recommendation")
	}
}

func TestFlashSplitWeightsCountAllPartsAndExcludeDraftModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"ggml-org/Qwen3.8-Flash-Next-GGUF","siblings":[{"rfilename":"Flash-Q2_K-00001-of-00002.gguf","size":10000000},{"rfilename":"Flash-Q2_K-00002-of-00002.gguf","size":83000000000},{"rfilename":"dflash-Flash-Q4_0.gguf","size":100},{"rfilename":"mtp-Flash-Q4_0.gguf","size":200}]}`)
	}))
	defer server.Close()
	c := &Client{HTTP: server.Client(), BaseURL: server.URL}
	d, err := c.Details(context.Background(), "ggml-org/Qwen3.8-Flash-Next-GGUF")
	if err != nil || len(d.Files) != 1 || d.Files[0].Bytes != 83010000000 || d.Files[0].UnavailableReason == "" {
		t.Fatalf("%+v %v", d, err)
	}
	for _, file := range []string{"mtp-Flash-Q4_0.gguf", "dflash-Flash-Q4_0.gguf"} {
		if _, err := ModelName("ggml-org/Flash", file); err == nil {
			t.Fatal("accepted sidecar")
		}
	}
}
