// Package agentpairverify verifies public test evidence independently of the
// ordinary replica probe. It does not commission peers or authorize a task.
package agentpairverify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
	"google.golang.org/protobuf/encoding/protojson"
)

const Kind = "UNTRUSTED-PRIVATE-AGENT-PAIR-PROOF-v1"
const MaxInput = 256 << 10

var ErrRefused = errors.New("paired public proof refused")

// Expectation is supplied by the test owner from public enrollment, commission
// and observed app lifecycle receipts, separately from the untrusted proof.
// It is not a production policy format or a claim about organization authority.
type Expectation struct {
	Kind       string              `json:"kind"`
	RootDER    []byte              `json:"root_der"`
	LeafSHA256 string              `json:"leaf_sha256"`
	KeySHA256  string              `json:"key_sha256"`
	Scope      serviceorigin.Scope `json:"scope"`
	Epoch      uint64              `json:"epoch"`
	Sequence   uint64              `json:"sequence"`
	NotBefore  time.Time           `json:"not_before"`
	NotAfter   time.Time           `json:"not_after"`
}

type Receipt struct {
	Kind               string `json:"kind"`
	Device             string `json:"device"`
	Generation         uint64 `json:"deployment_generation"`
	Epoch              uint64 `json:"epoch"`
	Sequence           uint64 `json:"sequence"`
	EnvelopeSHA256     string `json:"envelope_sha256"`
	RecordSHA256       string `json:"record_sha256"`
	RequestSHA256      string `json:"request_sha256"`
	LeafSHA256         string `json:"leaf_sha256"`
	ExactReceivedBytes bool   `json:"exact_received_bytes"`
	TimeEvidence       string `json:"authority_time_evidence,omitempty"`
}

func digest(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
func validDigest(v string) bool {
	b, err := hex.DecodeString(v)
	return err == nil && len(b) == 32 && hex.EncodeToString(b) == v
}

// Apply the fixture's narrower interval to every required time observation,
// including the verifier's final observation after its authorization callback.
type policyClock struct {
	provider      trustedtime.Provider
	before, after time.Time
}

func (c policyClock) Now() (trustedtime.Interval, error) {
	w, err := c.provider.Now()
	if err != nil || w.Validate() != nil || w.Earliest.Before(c.before) || !w.Latest.Before(c.after) {
		return trustedtime.Interval{}, ErrRefused
	}
	return w, nil
}

// Verify checks the original device delegation and service signature using
// independently authenticated current physical time. It compares the exact
// received envelope with the sender's envelope before parsing either. A nil
// received value checks only the original acknowledgment. It does not establish
// sample freshness, receiver authority, route choice or durable admission.
func Verify(ctx context.Context, expected Expectation, request, origin, received []byte, clock trustedtime.Provider) (Receipt, error) {
	if ctx == nil || ctx.Err() != nil || clock == nil || expected.Kind != Kind || expected.Scope.Validate() != nil || expected.Scope.ExecutionDomain != "simulation" || expected.Scope.App != "com.wendy.world.agentpair" || expected.Scope.Service != "probe" || len(expected.Scope.Publish) != 1 || expected.Scope.Publish[0] != (serviceorigin.PublishScope{ComponentType: "wendy.sensor.scalar.v1", Stream: "gas"}) || expected.Epoch == 0 || expected.Sequence == 0 || !validDigest(expected.LeafSHA256) || !validDigest(expected.KeySHA256) || expected.NotBefore.IsZero() || !expected.NotAfter.After(expected.NotBefore) || expected.NotAfter.Sub(expected.NotBefore) > 24*time.Hour || len(expected.RootDER) == 0 || len(expected.RootDER) > wendycert.MaxCertificateBytes || len(request) == 0 || len(request) > MaxInput || len(origin) == 0 || len(origin) > MaxInput || received != nil && !bytes.Equal(origin, received) {
		return Receipt{}, ErrRefused
	}
	// Own every mutable input before invoking a clock or authorization callback.
	// Callers must not write concurrently with this initial copy.
	expected.RootDER = bytes.Clone(expected.RootDER)
	expected.Scope.Publish = append([]serviceorigin.PublishScope(nil), expected.Scope.Publish...)
	request, origin, received = bytes.Clone(request), bytes.Clone(origin), bytes.Clone(received)
	clock = policyClock{provider: clock, before: expected.NotBefore, after: expected.NotAfter}
	var signed serviceorigin.SignedRecord
	if authority.DecodeStrictLimit(origin, &signed, MaxInput) != nil {
		return Receipt{}, ErrRefused
	}
	canonical, err := json.Marshal(signed)
	if err != nil || !bytes.Equal(canonical, origin) {
		return Receipt{}, ErrRefused
	}
	var wire appv1.PublishRequest
	if protojson.Unmarshal(request, &wire) != nil {
		return Receipt{}, ErrRefused
	}
	original, err := appv1.DecodePublish(&wire)
	if err != nil {
		return Receipt{}, ErrRefused
	}
	certificates, err := wendycert.NewDevice(wendycert.Config{Tenant: expected.Scope.Tenant, Roots: [][]byte{expected.RootDER}})
	if err != nil {
		return Receipt{}, ErrRefused
	}
	verifier, err := serviceorigin.NewVerifier(serviceorigin.VerifierConfig{Tenant: expected.Scope.Tenant, Site: expected.Scope.Site, ExecutionDomain: "simulation", Clock: clock, Certificates: certificates, Authorize: func(ctx context.Context, claims serviceorigin.DelegationClaims, device wendycert.DeviceIdentity) error {
		window, err := clock.Now()
		if ctx.Err() != nil || err != nil || window.Validate() != nil || window.Earliest.Before(expected.NotBefore) || !window.Latest.Before(expected.NotAfter) || !reflect.DeepEqual(claims.Scope, expected.Scope) || claims.Epoch != expected.Epoch || device.CertificateSHA256 != expected.LeafSHA256 || device.SigningKeySHA256 != expected.KeySHA256 {
			return ErrRefused
		}
		return nil
	}})
	if err != nil {
		return Receipt{}, ErrRefused
	}
	verified, err := verifier.Verify(ctx, signed)
	if err != nil || ctx.Err() != nil || verified.Record.Sequence != expected.Sequence || !reflect.DeepEqual(verified.Record.Publication, original) {
		return Receipt{}, ErrRefused
	}
	return Receipt{Kind: Kind, Device: expected.Scope.Device, Generation: expected.Scope.DeploymentGeneration, Epoch: expected.Epoch, Sequence: expected.Sequence, EnvelopeSHA256: digest(origin), RecordSHA256: digest(signed.Record), RequestSHA256: digest(request), LeafSHA256: verified.Device.CertificateSHA256, ExactReceivedBytes: received != nil}, nil
}

func DecodeExpectation(raw []byte) (Expectation, error) {
	var v Expectation
	if len(raw) == 0 || len(raw) > MaxInput || jws.DecodeStrict(raw, &v) != nil {
		return v, ErrRefused
	}
	return v, nil
}
