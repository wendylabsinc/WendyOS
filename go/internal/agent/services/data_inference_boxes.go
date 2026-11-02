package services

import "github.com/wendylabsinc/wendy/go/internal/agent/inference"

// predictionDetection is one detection in a campaign prediction record's
// attributes.detections, as Wendy Data records it in events.jsonl.
type predictionDetection struct {
	Label string  `json:"label"`
	Score float64 `json:"score"`
	// Box is the model's box unchanged: x1, y1, x2, y2, the top-left and
	// bottom-right corners, in pixels of the scored picture.
	Box [4]float64 `json:"box"`
	// BoxNorm is Box clamped to the picture and divided by its width and
	// height, in the same corner order, so every value is in [0, 1]. (0, 0)
	// is the top-left corner of the top-left pixel and (1, 1) the bottom-right
	// corner of the bottom-right pixel, x to the right and y down. It is
	// absent when the picture's size is unknown, or when the box has no area
	// inside the picture.
	BoxNorm *[4]float32 `json:"box_norm,omitempty"`
}

// addPicture puts the scored picture into a prediction record's attributes:
// its width and height in pixels, and each detection's box normalised against
// it as box_norm, next to the pixel box. A picture of unknown size adds
// neither, and the detections keep only their pixel boxes.
func addPicture(attributes map[string]any, detections []inference.Detection, width, height int) {
	recorded := make([]predictionDetection, 0, len(detections))
	for _, d := range detections {
		p := predictionDetection{Label: d.Label, Score: d.Score, Box: d.Box}
		if norm, ok := normalisedBox(d.Box, width, height); ok {
			p.BoxNorm = &norm
		}
		recorded = append(recorded, p)
	}
	attributes["detections"] = recorded
	if width > 0 && height > 0 {
		attributes["width"], attributes["height"] = width, height
	}
}

// normalisedBox clamps a corner box in pixels of a width by height picture to
// the picture and divides it by the picture's size. It is false for a picture
// of unknown size, and for a box with no area inside the picture.
func normalisedBox(box [4]float64, width, height int) ([4]float32, bool) {
	if width <= 0 || height <= 0 {
		return [4]float32{}, false
	}
	w, h := float64(width), float64(height)
	clamp := func(v, hi float64) float64 { return min(max(v, 0), hi) }
	x1, y1 := clamp(box[0], w), clamp(box[1], h)
	x2, y2 := clamp(box[2], w), clamp(box[3], h)
	if x2 <= x1 || y2 <= y1 {
		return [4]float32{}, false
	}
	return [4]float32{float32(x1 / w), float32(y1 / h), float32(x2 / w), float32(y2 / h)}, true
}
