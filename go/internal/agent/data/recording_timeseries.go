package data

import (
	"errors"
	"fmt"
	"math"
	"mime"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	recordingpb "github.com/wendylabsinc/wendy/go/proto/gen/recordingpb"
	"google.golang.org/protobuf/proto"
)

// Only the explicitly selected built-in schema is decoded on ingest. Other
// media types and schemas are retained verbatim for downstream consumers.
func validateTypedSampleBatch(cfg appconfig.RecordingStream, r *recordingpb.Record) error {
	mt, _, err := mime.ParseMediaType(cfg.MediaType)
	if err != nil || mt != "application/protobuf" || cfg.TimeSeries == nil {
		return errors.New("TimeSeriesBatch needs application/protobuf and timeSeries configuration")
	}
	batch := new(recordingpb.TimeSeriesBatch)
	if err := proto.Unmarshal(r.Payload, batch); err != nil {
		return fmt.Errorf("invalid TimeSeriesBatch: %w", err)
	}
	if batch.Timing == nil {
		return errors.New("TimeSeriesBatch needs timing")
	}
	if err := ValidateSampleTiming(batch.Timing); err != nil {
		return err
	}
	if batch.Timing.Clock != cfg.TimeSeries.Clock {
		return errors.New("TimeSeriesBatch clock differs from configuration")
	}
	if r.Timing != nil && !proto.Equal(r.Timing, batch.Timing) {
		return errors.New("envelope and TimeSeriesBatch timing differ")
	}
	if len(batch.Columns) != len(cfg.TimeSeries.Channels) {
		return errors.New("TimeSeriesBatch column count differs from configuration")
	}
	for i, column := range batch.Columns {
		if column == nil {
			return errors.New("missing TimeSeriesBatch column")
		}
		count := -1
		typ := cfg.TimeSeries.Channels[i].Type
		switch v := column.Values.(type) {
		case *recordingpb.SampleColumn_Float32Values:
			if typ == "float32" {
				count = len(v.Float32Values.Values)
			}
		case *recordingpb.SampleColumn_Float64Values:
			if typ == "float64" {
				count = len(v.Float64Values.Values)
			}
		case *recordingpb.SampleColumn_IntValues:
			if typ == "int64" || typ == "int32" {
				count = len(v.IntValues.Values)
			}
			if typ == "int32" {
				for _, n := range v.IntValues.Values {
					if n < math.MinInt32 || n > math.MaxInt32 {
						return errors.New("sample exceeds int32 range")
					}
				}
			}
		case *recordingpb.SampleColumn_UintValues:
			if typ == "uint64" || typ == "uint32" {
				count = len(v.UintValues.Values)
			}
			if typ == "uint32" {
				for _, n := range v.UintValues.Values {
					if n > math.MaxUint32 {
						return errors.New("sample exceeds uint32 range")
					}
				}
			}
		case *recordingpb.SampleColumn_BoolValues:
			if typ == "bool" {
				count = len(v.BoolValues.Values)
			}
		}
		if count != int(batch.Timing.Count) {
			return fmt.Errorf("channel %s: values must match configured type and sample count", cfg.TimeSeries.Channels[i].Name)
		}
	}
	return nil
}
