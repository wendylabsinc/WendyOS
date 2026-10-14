package mcp

import "slices"

// watchDetector is one detector a chat watch may run: a model run as its
// publisher released it, pinned to a commit. Neither the LLM nor the user can
// name another model or a URL through the watch tools.
//
// An entry needs, recorded here (spec §11.2): a Hugging Face repository
// pinned to a commit; a licence that allows commercial use; safetensors
// weights the Transformers backend loads without remote code; its labels as
// that commit's id2label gives them, in class order; and an on-device result
// of at least 1.5 frames per second per watch with two watches on the Orin
// Nano CPU at the worker's default 2 threads.
type watchDetector struct {
	ID        string
	Model     string
	Revision  string
	License   string
	Weights   string // the safetensors/Transformers/no-remote-code check
	Benchmark string
	Labels    []string
}

// watchDetectors is the vetted list, in order of preference: a watch runs the
// first detector whose labels cover all its classes. Open-vocabulary
// detectors are too heavy for the CPU budget and wait for the GPU backend.
var watchDetectors = []watchDetector{{
	ID:        "dfine-nano-coco",
	Model:     "ustc-community/dfine-nano-coco",
	Revision:  "066438d3d8f0da137a37b38fdf3368fd4afceced",
	License:   "Apache-2.0",
	Weights:   "safetensors; transformers backend; trust_remote_code off",
	Benchmark: "Orin Nano CPU, 2 threads: 1.90 fps alone, 1.65-1.74 fps each with two watches (2026-10-07)",
	Labels: []string{
		"person", "bicycle", "car", "motorbike", "aeroplane", "bus", "train", "truck", "boat",
		"traffic light", "fire hydrant", "stop sign", "parking meter", "bench", "bird", "cat", "dog",
		"horse", "sheep", "cow", "elephant", "bear", "zebra", "giraffe", "backpack", "umbrella",
		"handbag", "tie", "suitcase", "frisbee", "skis", "snowboard", "sports ball", "kite",
		"baseball bat", "baseball glove", "skateboard", "surfboard", "tennis racket", "bottle",
		"wine glass", "cup", "fork", "knife", "spoon", "bowl", "banana", "apple", "sandwich", "orange",
		"broccoli", "carrot", "hot dog", "pizza", "donut", "cake", "chair", "sofa", "pottedplant", "bed",
		"diningtable", "toilet", "tvmonitor", "laptop", "mouse", "remote", "keyboard", "cell phone",
		"microwave", "oven", "toaster", "sink", "refrigerator", "book", "clock", "vase", "scissors",
		"teddy bear", "hair drier", "toothbrush",
	},
}}

// detectorFor is the first vetted detector whose labels include every class.
func detectorFor(classes []string) (watchDetector, bool) {
	for _, d := range watchDetectors {
		if !slices.ContainsFunc(classes, func(class string) bool { return !d.hasLabel(class) }) {
			return d, true
		}
	}
	return watchDetector{}, false
}

func (d watchDetector) hasLabel(label string) bool { return slices.Contains(d.Labels, label) }
