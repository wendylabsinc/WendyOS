// Package missionclock defines simulation-gateway-relative mission and stop time.
// Its bounds assume a suspend-inclusive monotonic clock with at most 100ppm
// drift. A gateway signature is not evidence of UTC accuracy.
package missionclock

import (
	"encoding/json"
	"errors"
	"regexp"
	"time"
)

const (
	Version              = 1
	MaxRTT               = 2 * time.Second
	MaxHoldover          = 5 * time.Second
	DriftPPM       int64 = 100
	QuantizationMS int64 = 1
	Reference            = "simulation-gateway-relative-mission-v1"
)

var noncePattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type Request struct {
	Tenant      string `json:"tenant"`
	Site        string `json:"site"`
	Audience    string `json:"audience"`
	RunID       string `json:"run_id"`
	ClockDomain string `json:"clock_domain"`
	Purpose     string `json:"purpose"`
	Nonce       string `json:"nonce"`
}
type SignedRequest struct {
	Version   int             `json:"version"`
	Grant     json.RawMessage `json:"grant"`
	Request   Request         `json:"request"`
	Signature []byte          `json:"signature"`
}
type Observation struct {
	RequestSHA256     string `json:"request_sha256"`
	Tenant            string `json:"tenant"`
	Site              string `json:"site"`
	Audience          string `json:"audience"`
	RunID             string `json:"run_id"`
	ClockDomain       string `json:"clock_domain"`
	Purpose           string `json:"purpose"`
	Nonce             string `json:"nonce"`
	OperatorPrincipal string `json:"operator_principal"`
	OperatorKeySHA256 string `json:"operator_key_sha256"`
	GatewayPrincipal  string `json:"gateway_principal"`
	GatewayBootID     string `json:"gateway_boot_id"`
	SampleUnixMS      int64  `json:"sample_unix_ms"`
	Reference         string `json:"reference"`
	MaxRTTMS          int64  `json:"max_rtt_ms"`
	MaxHoldoverMS     int64  `json:"max_holdover_ms"`
	DriftPPM          int64  `json:"drift_ppm"`
	QuantizationMS    int64  `json:"quantization_ms"`
}
type SignedObservation struct {
	Version     int             `json:"version"`
	Grant       json.RawMessage `json:"grant"`
	Observation Observation     `json:"observation"`
	Signature   []byte          `json:"signature"`
}
type Evidence struct {
	Request     SignedRequest     `json:"request"`
	Observation SignedObservation `json:"observation"`
	RTTNS       int64             `json:"rtt_ns"`
	ElapsedNS   int64             `json:"elapsed_ns"`
}
type Interval struct {
	LowerUnixMS int64 `json:"lower_unix_ms"`
	UpperUnixMS int64 `json:"upper_unix_ms"`
}
type Pins struct {
	RootPublicKey    []byte `json:"root_public_key"`
	GatewayPrincipal string `json:"gateway_principal"`
	GatewayKeySHA256 string `json:"gateway_key_sha256"`
	RunID            string `json:"run_id"`
	ClockDomain      string `json:"clock_domain"`
}

func (r Request) Validate() error {
	for _, s := range []string{r.Tenant, r.Site, r.Audience, r.RunID, r.ClockDomain} {
		if s == "" || len(s) > 256 {
			return errors.New("invalid mission clock scope")
		}
	}
	if (r.Purpose != "mission" && r.Purpose != "stop") || r.ClockDomain != "simulation:"+r.RunID || !noncePattern.MatchString(r.Nonce) {
		return errors.New("invalid mission clock domain or nonce")
	}
	return nil
}
func (o Observation) Validate() error {
	if (Request{Tenant: o.Tenant, Site: o.Site, Audience: o.Audience, RunID: o.RunID, ClockDomain: o.ClockDomain, Purpose: o.Purpose, Nonce: o.Nonce}).Validate() != nil || !noncePattern.MatchString(o.RequestSHA256) || !noncePattern.MatchString(o.OperatorKeySHA256) || !noncePattern.MatchString(o.GatewayBootID) || o.OperatorPrincipal == "" || len(o.OperatorPrincipal) > 256 || o.GatewayPrincipal == "" || len(o.GatewayPrincipal) > 256 || o.SampleUnixMS <= 0 || o.SampleUnixMS > 253402214400000 || o.Reference != Reference || o.MaxRTTMS != MaxRTT.Milliseconds() || o.MaxHoldoverMS != MaxHoldover.Milliseconds() || o.DriftPPM != DriftPPM || o.QuantizationMS != QuantizationMS {
		return errors.New("invalid mission clock observation")
	}
	return nil
}

// Estimate includes the entire RTT because the signed gateway sample is taken
// somewhere between send and receipt. Bounds round outward to milliseconds.
func Estimate(o Observation, rtt, elapsed time.Duration) (Interval, error) {
	if o.Validate() != nil || rtt < 0 || rtt > MaxRTT || elapsed < 0 || elapsed > MaxHoldover {
		return Interval{}, errors.New("mission clock bound unavailable")
	}
	ceilMS := func(n int64) int64 { return (n + int64(time.Millisecond) - 1) / int64(time.Millisecond) }
	driftNS := (int64(rtt+elapsed)*DriftPPM + 999999) / 1000000
	lower := o.SampleUnixMS + int64(elapsed/time.Millisecond) - ceilMS(driftNS) - QuantizationMS
	upper := o.SampleUnixMS + ceilMS(int64(rtt+elapsed)) + ceilMS(driftNS) + QuantizationMS
	return Interval{lower, upper}, nil
}
func (e Evidence) Interval() (Interval, error) {
	return Estimate(e.Observation.Observation, time.Duration(e.RTTNS), time.Duration(e.ElapsedNS))
}
