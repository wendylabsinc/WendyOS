package models

import (
	"strings"
	"testing"
)

const testSHA = "0000000000000000000000000000000000000000000000000000000000000001"

func testFile() string {
	return `{"url": "https://storage.googleapis.com/wendy-models-public/sha256/` + testSHA + `", "sha256": "` + testSHA + `", "bytes": 1024}`
}

// validCatalogJSON has one detector with a QNN, a TensorRT and a CPU variant,
// in that order.
func validCatalogJSON() string {
	return `{
  "version": "test-1",
  "models": [{
    "id": "coco-detector",
    "description": "Detects the 80 COCO classes",
    "kind": "detector",
    "labels": ["person", "car", "traffic light"],
    "variants": [
      {"id": "d-qnn", "engine": "qnn", "requires": {"npu_backend": "qnn"},
       "host_image": "ghcr.io/wendylabsinc/wendy-model-host-qualcomm@sha256:` + strings.Repeat("a", 64) + `",
       "file": ` + testFile() + `, "input_size": 640},
      {"id": "d-trt", "engine": "tensorrt", "requires": {"gpu_vendor": "nvidia", "compute_backend": "cuda"},
       "host_image": "ghcr.io/wendylabsinc/wendy-model-host-jetson@sha256:` + strings.Repeat("b", 64) + `",
       "file": ` + testFile() + `, "input_size": 640},
      {"id": "d-cpu", "engine": "onnxruntime", "requires": {"arch": "arm64"},
       "host_image": "ghcr.io/wendylabsinc/wendy-model-host-cpu@sha256:` + strings.Repeat("c", 64) + `",
       "file": ` + testFile() + `, "input_size": 320}
    ]
  }]
}`
}

func TestParseCatalogAcceptsValidCatalog(t *testing.T) {
	c, err := ParseCatalog([]byte(validCatalogJSON()))
	if err != nil {
		t.Fatal(err)
	}
	m, ok := c.Model("coco-detector")
	if !ok || len(m.Variants) != 3 || m.Variants[2].InputSize != 320 {
		t.Fatalf("model = %+v, %v", m, ok)
	}
	if _, ok := c.Model("missing"); ok {
		t.Fatal("found a model that is not in the catalog")
	}
}

func TestDefaultCatalogParses(t *testing.T) {
	c, err := DefaultCatalog()
	if err != nil {
		t.Fatalf("the embedded catalog is invalid: %v", err)
	}
	if c.Version == "" {
		t.Fatal("the embedded catalog has no version")
	}
}

func TestParseCatalogRejects(t *testing.T) {
	cases := map[string]func(string) string{
		"tag-only image": func(s string) string {
			return strings.Replace(s, "@sha256:"+strings.Repeat("c", 64), ":latest", 1)
		},
		"http file url": func(s string) string { return strings.Replace(s, "https://", "http://", 1) },
		"unknown field": func(s string) string {
			return strings.Replace(s, `"input_size": 320`, `"input_size": 320, "extra": 1`, 1)
		},
		"unknown engine": func(s string) string { return strings.Replace(s, `"engine": "onnxruntime"`, `"engine": "tflite"`, 1) },
		"repeated label": func(s string) string { return strings.Replace(s, `"car"`, `"person"`, 1) },
		"short digest":   func(s string) string { return strings.Replace(s, `"sha256": "`+testSHA+`"`, `"sha256": "abc"`, 1) },
		"no version":     func(s string) string { return strings.Replace(s, `"version": "test-1"`, `"version": ""`, 1) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseCatalog([]byte(mutate(validCatalogJSON()))); err == nil {
				t.Fatal("accepted an invalid catalog")
			}
		})
	}
}
