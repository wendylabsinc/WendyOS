// Package gascontract defines the commissioned simulation model independently of authority and transport.
package gascontract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/gasfield"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

type Config struct {
	Tenant      string                  `json:"tenant"`
	Anchor      time.Time               `json:"anchor"`
	Field       gasfield.Config         `json:"field"`
	Assumptions gasfield.Assumptions    `json:"assumptions"`
	Gateways    map[string]world.Source `json:"gateways"`
}

func (c Config) Validate() error {
	if c.Tenant == "" || len(c.Tenant) > 128 || c.Anchor.IsZero() || len(c.Gateways) != 2 {
		return errors.New("explicit two-gateway simulation scope required")
	}
	if _, err := new(gasfield.Session).Step(gasfield.Request{Config: &c.Field, Now: 0, Assumptions: c.Assumptions}); err != nil {
		return err
	}
	seen := map[world.Source]bool{}
	for name, source := range c.Gateways {
		if len(name) < 1 || len(name) > 64 || source.Validate() != nil || seen[source] {
			return errors.New("invalid gateway catalog")
		}
		seen[source] = true
	}
	for _, sensor := range c.Field.Sensors {
		if seen[sensor.Source] {
			return errors.New("a derived publisher cannot be a raw sensor")
		}
	}
	return nil
}
func (c Config) Instant(seconds float64) time.Time {
	return c.Anchor.Add(time.Duration(math.Round(seconds * 1e9)))
}
func (c Config) Seconds(at time.Time) float64 { return at.Sub(c.Anchor).Seconds() }
func digest(value any) string {
	raw, _ := json.Marshal(value)
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}
func (c Config) ContractDigest() string { return digest(c) }
