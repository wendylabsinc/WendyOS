package fusion

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// PublicationJSON preserves input proof/source/version/time references while
// avoiding a second inline copy of raw LiDAR scans in a derived claim. The
// original full scan belongs to the referenced input capture, not this result.
// The in-memory Hazard and local demo output retain their complete samples.
func PublicationJSON(hazard *Hazard) ([]byte, error) {
	raw, err := json.Marshal(hazard)
	if err != nil {
		return nil, err
	}
	hasLidar := false
	for _, input := range hazard.Inputs {
		hasLidar = hasLidar || input.Sample.Lidar != nil
	}
	if !hasLidar {
		return raw, nil
	}
	var result map[string]json.RawMessage
	if err = json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	var inputs []map[string]json.RawMessage
	if err = json.Unmarshal(result["inputs"], &inputs); err != nil {
		return nil, err
	}
	for i, input := range hazard.Inputs {
		if input.Sample.Lidar == nil {
			continue
		}
		sample, err := json.Marshal(input.Sample)
		if err != nil {
			return nil, err
		}
		hash := sha256.Sum256(sample)
		summary := struct {
			PointCount             int     `json:"point_count"`
			MaxRangeM              float64 `json:"max_range_m"`
			NormalizedSampleSHA256 string  `json:"normalized_sample_sha256"`
		}{len(input.Sample.Lidar.PointsM), input.Sample.Lidar.MaxRangeM, hex.EncodeToString(hash[:])}
		encoded, err := json.Marshal(summary)
		if err != nil {
			return nil, err
		}
		delete(inputs[i], "sample")
		inputs[i]["lidar_summary"] = encoded
	}
	result["inputs"], err = json.Marshal(inputs)
	if err != nil {
		return nil, err
	}
	// Display at most32 support points; the explicit total still describes all
	// matched measurements used for the location and uncertainty calculation.
	if hazard.Spatial != nil && len(hazard.Spatial.LidarSupportPoints) > 32 {
		view := *hazard.Spatial
		view.LidarSupportPoints = view.LidarSupportPoints[:32]
		result["spatial"], err = json.Marshal(view)
		if err != nil {
			return nil, err
		}
	}
	return json.Marshal(result)
}
