package data

import "strings"

// ContentTypes summarizes saved payloads using the existing manifest file metadata.
func ContentTypes(files []File) []string {
	found := map[string]bool{}
	for _, f := range files {
		p, media := strings.ToLower(f.Path), strings.ToLower(f.MediaType)
		switch {
		case strings.HasPrefix(media, "video/") || strings.HasSuffix(p, ".mp4") || strings.HasSuffix(p, ".h264") || strings.HasSuffix(p, ".h265") || strings.HasSuffix(p, ".hevc") || strings.HasSuffix(p, ".mov") || strings.HasSuffix(p, ".mjpeg"):
			found["video"] = true
		case strings.HasPrefix(media, "image/") || strings.HasSuffix(p, ".jpg") || strings.HasSuffix(p, ".jpeg") || strings.HasSuffix(p, ".png"):
			found["image"] = true
		case strings.HasPrefix(media, "audio/") || strings.HasSuffix(p, ".wav"):
			found["audio"] = true
		case p == "telemetry.jsonl":
			found["device_stats"] = true
		case p == "events.jsonl":
			found["events"] = true
		case strings.HasPrefix(p, "ros2/") && (strings.HasSuffix(p, ".db3") || strings.HasSuffix(p, ".mcap")):
			found["robot_data"] = true
		}
	}
	out := []string{}
	for _, kind := range []string{"video", "image", "audio", "device_stats", "events", "robot_data"} {
		if found[kind] {
			out = append(out, kind)
		}
	}
	return out
}
