package twin

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
	"math"
	"strings"
	"time"
)

type ReferenceImport struct {
	ModelRevision string            `json:"modelRevision"`
	ModelDigest   string            `json:"modelDigest"`
	Robot         string            `json:"robot"`
	SavedAt       time.Time         `json:"savedAt"`
	Surfaces      []sitenav.Surface `json:"surfaces"`
}

// ImportReferences fills a missing legacy reference map, never replacing newer
// Linux observations. Frame and model bytes must match the original capture.
func (s *Store) ImportReferences(id string, input ReferenceImport) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.read(id)
	if err != nil {
		return err
	}
	if r.Removed || !strings.EqualFold(r.Site.ModelRevision, input.ModelRevision) {
		return errors.New("reference frame does not match room")
	}
	age := time.Since(input.SavedAt)
	if input.Robot == "" || len(input.Robot) > 256 || age < 0 || age > 7*24*time.Hour || len(input.Surfaces) == 0 || len(input.Surfaces) > 3000 {
		return errors.New("invalid legacy reference map")
	}
	r, err = s.assets(r)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(r.Model)
	if !r.HasModel || hex.EncodeToString(digest[:]) != input.ModelDigest {
		return errors.New("reference model digest does not match room")
	}
	for _, p := range input.Surfaces {
		for _, v := range []float64{p.Position.X, p.Position.Y, p.Position.Z, p.Direction.X, p.Direction.Y} {
			if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > 1000 {
				return errors.New("invalid reference surface")
			}
		}
		if !p.Horizontal && math.Abs(math.Hypot(p.Direction.X, p.Direction.Y)-1) > .05 {
			return errors.New("invalid reference direction")
		}
	}
	if len(r.References[input.Robot]) > 0 {
		return nil
	}
	if r.References == nil {
		r.References = map[string][]sitenav.Surface{}
	}
	r.References[input.Robot] = input.Surfaces
	r.Revision++
	return s.write(r)
}
