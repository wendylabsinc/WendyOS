// Package replicaprobe exercises native repair through an ordinary service
// socket. It holds no signing key and produces only synthetic scalar samples.
package replicaprobe

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/appclock"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

const (
	App              = "com.wendy.world.agentpair"
	Service          = "probe"
	Component        = "wendy.sensor.scalar.v1"
	Stream           = "gas"
	Entity           = "replica-probe"
	MaxConfigBytes   = 16 << 10
	MaxCommandBytes  = 1024
	MaxEnvelopeBytes = 256 << 10
	MaxReceiptBytes  = 512 << 10
	MaxCommands      = 32
	MaxAwaitAttempts = 32
	AwaitBudget      = 10 * time.Second
	AwaitInterval    = 200 * time.Millisecond
)

var (
	ErrConfig    = errors.New("replica probe configuration refused")
	ErrCommand   = errors.New("replica probe command refused")
	ErrResponse  = errors.New("replica probe response refused")
	ErrUncertain = errors.New("publication outcome unknown; no retry")
	ErrStopped   = errors.New("replica probe stopped")
	ErrAwait     = errors.New("query expectation not observed within attempt bound")
)

type Config struct {
	Version            int            `json:"version"`
	Tenant             string         `json:"tenant"`
	Site               string         `json:"site"`
	Device             string         `json:"device"`
	PeerDevice         string         `json:"peer_device"`
	RunID              string         `json:"run_id"`
	ScenarioVersion    string         `json:"scenario_version"`
	Seed               uint64         `json:"seed"`
	Frame              world.FrameRef `json:"frame"`
	SampleClockDomain  string         `json:"sample_clock_domain"`
	ExpectedGeneration uint64         `json:"expected_generation,omitempty"`
	DurationMS         int64          `json:"duration_ms"`
	MaxCommands        int            `json:"max_commands"`
	RequestTimeoutMS   int64          `json:"request_timeout_ms,omitempty"`
	ClockMaxWidthMS    int64          `json:"clock_max_width_ms,omitempty"`
	PollIntervalMS     int64          `json:"poll_interval_ms,omitempty"`
}

func (c Config) normalized() (Config, error) {
	if c.RequestTimeoutMS == 0 {
		c.RequestTimeoutMS = 1000
	}
	if c.ClockMaxWidthMS == 0 {
		c.ClockMaxWidthMS = 5000
	}
	if c.PollIntervalMS == 0 {
		c.PollIntervalMS = 100
	}
	scope := serviceorigin.Scope{Tenant: c.Tenant, Site: c.Site, Device: c.Device, App: App, Service: Service, DeploymentGeneration: max(c.ExpectedGeneration, 1), ExecutionDomain: "simulation"}
	peer, e := serviceaccess.DeviceTenant(c.PeerDevice)
	provenance := world.Provenance{Origin: world.Simulated, AppVersion: "replica-probe-v1", Simulation: &world.SimulationContext{RunID: c.RunID, ScenarioVersion: c.ScenarioVersion, Seed: c.Seed, ClockDomain: c.SampleClockDomain}}
	if c.Version != 1 || scope.Validate() != nil || e != nil || peer != c.Tenant || c.PeerDevice == c.Device || c.Frame.Validate() != nil || provenance.Validate() != nil || !boundedText(c.RunID, 128) || !boundedText(c.ScenarioVersion, 128) || !boundedText(c.SampleClockDomain, 256) || !strings.HasPrefix(c.SampleClockDomain, "simulation:") || len(c.SampleClockDomain) == len("simulation:") || c.Seed >= 1<<32 || c.DurationMS < 100 || c.DurationMS > 600000 || c.MaxCommands < 1 || c.MaxCommands > MaxCommands || c.RequestTimeoutMS < 10 || c.RequestTimeoutMS > 5000 || c.ClockMaxWidthMS < 1 || c.ClockMaxWidthMS > 5000 || c.PollIntervalMS < 10 || c.PollIntervalMS > 1000 {
		return Config{}, ErrConfig
	}
	return c, nil
}

func boundedText(s string, limit int) bool {
	if s == "" || len(s) > limit || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r <= 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func (c Config) clockConfig() appclock.Config {
	return appclock.Config{Tenant: c.Tenant, Site: c.Site, Device: c.Device, App: App, Service: Service, ExecutionDomain: "simulation", SampleClockDomain: c.SampleClockDomain, ExpectedGeneration: c.ExpectedGeneration, MaxRoundTrip: min(time.Duration(c.RequestTimeoutMS)*time.Millisecond, time.Second), MaxWidth: time.Duration(c.ClockMaxWidthMS) * time.Millisecond, DriftPPM: 100}
}

// Command requests one operation. A query always starts a new latest-value
// snapshot; it does not turn an earlier empty result into a durable assertion.
type Command struct {
	Version        int      `json:"version"`
	Sequence       uint32   `json:"sequence"`
	Action         string   `json:"action"`
	Value          *float64 `json:"value,omitempty"`
	TTLMS          int64    `json:"ttl_ms,omitempty"`
	Expect         string   `json:"expect,omitempty"`
	EnvelopeSHA256 string   `json:"envelope_sha256,omitempty"`
}

func DecodeCommand(raw []byte) (Command, error) {
	var c Command
	if len(raw) > MaxCommandBytes || jws.DecodeStrict(raw, &c) != nil || c.Version != 1 || c.Sequence < 1 || c.Sequence > MaxCommands {
		return c, ErrCommand
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return c, ErrCommand
	}
	_, hasExpect := fields["expect"]
	_, hasEnvelope := fields["envelope_sha256"]
	if c.Action != "await_query" && (hasExpect || hasEnvelope) {
		return c, ErrCommand
	}
	switch c.Action {
	case "await_query":
		if c.Value != nil || c.TTLMS != 0 {
			return c, ErrCommand
		}
		switch c.Expect {
		case "ready", "absent":
			if hasEnvelope {
				return c, ErrCommand
			}
		case "present":
			d, e := hex.DecodeString(c.EnvelopeSHA256)
			if e != nil || len(d) != 32 || hex.EncodeToString(d) != c.EnvelopeSHA256 {
				return c, ErrCommand
			}
		default:
			return c, ErrCommand
		}
	case "publish":
		if c.Value == nil || math.IsNaN(*c.Value) || math.IsInf(*c.Value, 0) || *c.Value < 0 || *c.Value > 1000000 || c.TTLMS < 100 || c.TTLMS > 30000 {
			return c, ErrCommand
		}
	case "tombstone":
		if c.Value != nil || c.TTLMS < 100 || c.TTLMS > 30000 {
			return c, ErrCommand
		}
	case "query", "finish":
		if c.Value != nil || c.TTLMS != 0 {
			return c, ErrCommand
		}
	default:
		return c, ErrCommand
	}
	return c, nil
}

type ClockReceipt struct {
	SampleUnixNS           int64  `json:"sample_unix_ns"`
	SampleClockDomain      string `json:"sample_clock_domain"`
	DeploymentGeneration   uint64 `json:"deployment_generation"`
	ServerBootID           string `json:"server_boot_id"`
	PhysicalEarliestUnixNS int64  `json:"physical_earliest_unix_ns"`
	PhysicalLatestUnixNS   int64  `json:"physical_latest_unix_ns"`
	RoundTripNS            int64  `json:"round_trip_ns"`
}

type Original struct {
	Envelope       []byte `json:"envelope_json"`
	EnvelopeSHA256 string `json:"envelope_sha256"`
	RecordSHA256   string `json:"record_sha256"`
}

// QueryAttempt is bounded diagnostic evidence, not a renewed proof or lease.
type QueryAttempt struct {
	Attempt        int           `json:"attempt"`
	Clock          *ClockReceipt `json:"clock,omitempty"`
	RPCCode        string        `json:"rpc_code,omitempty"`
	Outcome        string        `json:"outcome"`
	ElapsedNS      int64         `json:"elapsed_ns"`
	ReplySHA256    string        `json:"reply_json_sha256,omitempty"`
	EnvelopeSHA256 string        `json:"envelope_sha256,omitempty"`
}

// Receipts bind returned bytes structurally to this exact socket/configuration.
// They are not an independent signature or historical-authority verification.
type Receipt struct {
	Version        int            `json:"version"`
	RunID          string         `json:"run_id"`
	Sequence       uint32         `json:"sequence"`
	Action         string         `json:"action"`
	Status         string         `json:"status"`
	ErrorCode      string         `json:"error,omitempty"`
	RPCCode        string         `json:"rpc_code,omitempty"`
	CommandJSON    []byte         `json:"command_json"`
	CommandSHA256  string         `json:"command_sha256"`
	Clock          *ClockReceipt  `json:"clock,omitempty"`
	RequestJSON    []byte         `json:"request_json,omitempty"`
	ReplyJSON      []byte         `json:"reply_json,omitempty"`
	Originals      []Original     `json:"originals"`
	Validation     string         `json:"validation"`
	Attempts       []QueryAttempt `json:"attempts,omitempty"`
	AwaitElapsedNS int64          `json:"await_elapsed_ns,omitempty"`
}

type Result struct {
	Version   int    `json:"version"`
	RunID     string `json:"run_id"`
	Status    string `json:"status"`
	ErrorCode string `json:"error,omitempty"`
	Completed uint32 `json:"completed"`
}
