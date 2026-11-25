// Package hazardreader reads current origin-attested multimodal hazards through
// the mounted World API. It does not fetch or independently interpret raw media.
package hazardreader

import (
	"context"
	"errors"
	"strings"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/fusioncapture"
	"github.com/wendylabsinc/wendy/go/world/production/internal/derivedreader"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

var ErrRefused = errors.New("current hazard read refused")
var ErrUnavailable = derivedreader.ErrUnavailable

type Source = derivedreader.Source
type Config derivedreader.Config
type Hazard = derivedreader.Item[fusioncapture.ReplicaHazard]
type Clock = derivedreader.Clock

type Result struct {
	Version                        int       `json:"version"`
	Tick                           int       `json:"tick"`
	RunID                          string    `json:"run_id"`
	SampleTime                     time.Time `json:"sample_time"`
	ReaderGeneration               uint64    `json:"reader_generation"`
	Status                         string    `json:"status"`
	RPCCode                        string    `json:"rpc_code,omitempty"`
	StartupAttempt                 int       `json:"startup_attempt,omitempty"`
	Clock                          *Clock    `json:"clock,omitempty"`
	Hazards                        []Hazard  `json:"hazards"`
	AbsentHazardIDs                []string  `json:"absent_hazard_ids,omitempty"`
	OriginAttestedInference        bool      `json:"origin_attested_inference"`
	RawInputsIndependentlyVerified bool      `json:"raw_inputs_independently_verified"`
}
type Summary struct {
	Ticks        int    `json:"ticks"`
	HazardsRead  int    `json:"hazards_read"`
	StartupWaits int    `json:"startup_waits"`
	Status       string `json:"status"`
}
type Session struct {
	reader *derivedreader.Session[fusioncapture.ReplicaHazard]
}

func mapped(err error) error {
	if errors.Is(err, derivedreader.ErrRefused) {
		return ErrRefused
	}
	return err
}
func LoadConfig(path string) (Config, error) {
	c, err := derivedreader.LoadConfig(path)
	return Config(c), mapped(err)
}
func decode(request serviceorigin.PublishRequest) (derivedreader.Interpretation[fusioncapture.ReplicaHazard], error) {
	r, err := fusioncapture.ValidateReplicaPublication(request)
	if err != nil {
		return derivedreader.Interpretation[fusioncapture.ReplicaHazard]{}, err
	}
	return derivedreader.Interpretation[fusioncapture.ReplicaHazard]{Payload: r.Hazard, ID: request.Entity, RunID: strings.TrimPrefix(request.Timing.ClockDomain, "simulation:"), Frame: request.Frame, SampleValidUntil: r.SampleValidUntil, PhysicalValidUntil: r.PhysicalValidUntil}, nil
}
func result(r derivedreader.Result[fusioncapture.ReplicaHazard]) Result {
	return Result{Version: r.Version, Tick: r.Tick, RunID: r.RunID, SampleTime: r.SampleTime, ReaderGeneration: r.ReaderGeneration, Status: r.Status, RPCCode: r.RPCCode, StartupAttempt: r.StartupAttempt, Clock: r.Clock, Hazards: r.Items, AbsentHazardIDs: r.AbsentIDs, OriginAttestedInference: r.OriginAttestedInference}
}
func New(config Config, api appv1.WorldAppServiceClient) (*Session, error) {
	s, err := derivedreader.New(derivedreader.Config(config), api, decode)
	if err != nil {
		return nil, mapped(err)
	}
	return &Session{reader: s}, nil
}

// NewContinuous retains source and clock history across caller-owned reads.
func NewContinuous(config Config, api appv1.WorldAppServiceClient) (*Session, error) {
	s, err := derivedreader.NewContinuous(derivedreader.Config(config), api, decode)
	if err != nil {
		return nil, mapped(err)
	}
	return &Session{reader: s}, nil
}
func (s *Session) Close() error {
	if s == nil {
		return nil
	}
	return s.reader.Close()
}
func (s *Session) Step(ctx context.Context) (Result, error) {
	r, err := s.reader.Step(ctx)
	return result(r), mapped(err)
}
func Run(ctx context.Context, config Config, api appv1.WorldAppServiceClient, emit func(Result) error) (Summary, error) {
	if emit == nil {
		return Summary{}, ErrRefused
	}
	r, err := derivedreader.Run(ctx, derivedreader.Config(config), api, decode, func(r derivedreader.Result[fusioncapture.ReplicaHazard]) error { return emit(result(r)) })
	return Summary{Ticks: r.Ticks, HazardsRead: r.ItemsRead, StartupWaits: r.StartupWaits, Status: r.Status}, mapped(err)
}
