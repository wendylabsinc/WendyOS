// Package patrolwatchpolicy defines signed mission policy without runtime readers or authority.
package patrolwatchpolicy

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"

	"github.com/wendylabsinc/wendy/go/world/internal/gascontract"
	"github.com/wendylabsinc/wendy/go/world/internal/strictjson"
)

const Method = "synthetic-source-region-outside-watch-v1"

type Policy struct {
	Method          string             `json:"method"`
	Contract        gascontract.Config `json:"contract"`
	ContractSHA256  string             `json:"contract_sha256"`
	ReaderPrincipal string             `json:"reader_principal"`
	ReaderAudience  string             `json:"reader_audience"`
	MinXZ           []float64          `json:"min_xz"`
	MaxXZ           []float64          `json:"max_xz"`
	MaxAgeMS        int64              `json:"max_age_ms"`
}

func (p Policy) Validate() error {
	if p.Method != Method || p.Contract.Validate() != nil || p.ContractSHA256 != p.Contract.ContractDigest() || p.Contract.Tenant != "simulation" || p.Contract.Field.Site != "studio-01" || p.ReaderPrincipal == "" || len(p.ReaderPrincipal) > 128 || p.ReaderAudience == "" || len(p.ReaderAudience) > 128 || len(p.MinXZ) != 2 || len(p.MaxXZ) != 2 || p.MaxAgeMS < 500 || p.MaxAgeMS > 4000 {
		return errors.New("world watch requires a bounded commissioned simulation contract, reader and original age limit")
	}
	for i := 0; i < 2; i++ {
		if math.IsNaN(p.MinXZ[i]) || math.IsInf(p.MinXZ[i], 0) || math.IsNaN(p.MaxXZ[i]) || math.IsInf(p.MaxXZ[i], 0) || p.MinXZ[i] >= p.MaxXZ[i] || p.MinXZ[i] < -1000 || p.MaxXZ[i] > 1000 {
			return errors.New("world watch requires finite explicit ordered X/Z corners")
		}
	}
	return nil
}
func (p *Policy) UnmarshalJSON(raw []byte) error {
	type plain Policy
	var fields map[string]json.RawMessage
	if e := json.Unmarshal(raw, &fields); e != nil {
		return e
	}
	for _, k := range []string{"method", "contract", "contract_sha256", "reader_principal", "reader_audience", "min_xz", "max_xz", "max_age_ms"} {
		if len(fields[k]) == 0 || bytes.Equal(bytes.TrimSpace(fields[k]), []byte("null")) {
			return errors.New("world watch requires every non-null policy field")
		}
	}
	for _, k := range []string{"min_xz", "max_xz"} {
		var v []*float64
		if json.Unmarshal(fields[k], &v) != nil || len(v) != 2 || v[0] == nil || v[1] == nil {
			return errors.New("world watch corners require two explicit numbers")
		}
	}
	var v plain
	if e := strictjson.DecodeLimit(raw, &v, 16384); e != nil {
		return e
	}
	*p = Policy(v)
	return p.Validate()
}
