package mcp

import "slices"

// watchDetector is the detector chat watches run (design §8, D5): a model run
// as its publisher released it, pinned to a commit. Neither the LLM nor the
// user can choose another model or a URL through the watch tools. Changing the
// default detector means changing this one entry.
type watchDetector struct {
	ID       string
	Model    string
	Revision string
	// Labels are the checkpoint's class names in class order, copied from its
	// config.json at Revision. A watch's classes must be among them.
	Labels []string
}

var defaultWatchDetector = watchDetector{
	ID:       "dfine-nano-coco",
	Model:    "ustc-community/dfine-nano-coco",
	Revision: "066438d3d8f0da137a37b38fdf3368fd4afceced",
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
}

func (d watchDetector) hasLabel(label string) bool { return slices.Contains(d.Labels, label) }
