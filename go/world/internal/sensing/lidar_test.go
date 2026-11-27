package sensing

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"testing"
)

func lidarFixture() Sample {
	s := sampleFixture()
	s.ID, s.SensorID, s.Kind = "run/lidar-1/1", "lidar-1", "lidar"
	s.Gas = nil
	s.Lidar = &Lidar{PointsM: [][3]float64{{1, 2, 3}, {-1.25, .2, 4}}, MaxRangeM: 6.1}
	return s
}

func TestLidarPreservesSiteCoordinatesAndEmptyScan(t *testing.T) {
	for _, points := range [][][3]float64{lidarFixture().Lidar.PointsM, {}, make([][3]float64, MaxLidarPoints)} {
		s := lidarFixture()
		s.Lidar.PointsM = points
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decode(raw)
		if err != nil || !reflect.DeepEqual(got, s) {
			t.Fatalf("site-frame cloud or sample metadata changed: %v", err)
		}
	}
}

func TestLidarStrictPointShapeAndMetadata(t *testing.T) {
	raw, _ := json.Marshal(lidarFixture())
	for name, replacement := range map[string]string{
		"short": `[[1,2]]`, "long": `[[1,2,3,4]]`, "null point": `[null]`,
		"null coordinate": `[[1,null,3]]`, "string coordinate": `[[1,"2",3]]`,
		"null cloud": `null`, "object cloud": `{}`, "overflow": `[[1e999,2,3]]`,
	} {
		t.Run(name, func(t *testing.T) {
			bad := bytes.Replace(raw, []byte(`[[1,2,3],[-1.25,0.2,4]]`), []byte(replacement), 1)
			if bytes.Equal(raw, bad) {
				t.Fatal("fixture replacement missed")
			}
			if _, err := Decode(bad); err == nil {
				t.Fatal("accepted malformed point cloud")
			}
		})
	}
	for name, mutate := range map[string]func([]byte) []byte{
		"missing points": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"points_m":[[1,2,3],[-1.25,0.2,4]],`), nil, 1)
		},
		"case alias": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"lidar":`), []byte(`"LIDAR":`), 1)
		},
		"nested case duplicate": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"max_range_m":6.1`), []byte(`"max_range_m":6.1,"MAX_RANGE_M":7`), 1)
		},
		"missing range": func(b []byte) []byte { return bytes.Replace(b, []byte(`,"max_range_m":6.1`), nil, 1) },
		"duplicate range": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"max_range_m":6.1`), []byte(`"max_range_m":6.1,"max_range_m":7`), 1)
		},
		"unknown field": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"max_range_m":6.1`), []byte(`"max_range_m":6.1,"axis":"z-up"`), 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(mutate(raw)); err == nil {
				t.Fatal("accepted malformed lidar metadata")
			}
		})
	}
}

func TestLidarBoundsAndExclusiveKind(t *testing.T) {
	for name, change := range map[string]func(*Sample){
		"nil cloud":       func(s *Sample) { s.Lidar.PointsM = nil },
		"too many points": func(s *Sample) { s.Lidar.PointsM = make([][3]float64, MaxLidarPoints+1) },
		"nan point":       func(s *Sample) { s.Lidar.PointsM[0][1] = math.NaN() },
		"infinite point":  func(s *Sample) { s.Lidar.PointsM[0][2] = math.Inf(1) },
		"zero range":      func(s *Sample) { s.Lidar.MaxRangeM = 0 },
		"negative range":  func(s *Sample) { s.Lidar.MaxRangeM = -1 },
		"infinite range":  func(s *Sample) { s.Lidar.MaxRangeM = math.Inf(1) },
		"gas mix":         func(s *Sample) { s.Gas = sampleFixture().Gas },
		"wrong kind":      func(s *Sample) { s.Kind, s.Gas = "gas", sampleFixture().Gas },
		"media":           func(s *Sample) { s.Evidence = &EvidenceRef{} },
		"sensorlink":      func(s *Sample) { s.SensorLink = &SensorLink{} },
		"sample byte budget": func(s *Sample) {
			s.Lidar.PointsM = make([][3]float64, MaxLidarPoints)
			for i := range s.Lidar.PointsM {
				s.Lidar.PointsM[i] = [3]float64{math.MaxFloat64, math.MaxFloat64, math.MaxFloat64}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := lidarFixture()
			change(&s)
			if err := s.Validate(); err == nil {
				t.Fatal("accepted invalid or over-budget lidar sample")
			}
		})
	}
}
