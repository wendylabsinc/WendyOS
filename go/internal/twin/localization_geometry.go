package twin

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot"
)

// RoomLocalizationGeometry is the metric, Y-up semantic mesh exported by the
// existing room renderer. Captured robot hints, search policy and observations
// are never cached as architectural geometry.
type RoomLocalizationGeometry struct {
	Frame        string    `json:"frame"`
	Architecture []float64 `json:"architecture"`
	Furniture    []float64 `json:"furniture,omitempty"`
}

func (s *Store) SaveLocalizationGeometry(input twinrobot.SiteFusionRequest) error {
	return s.withFrame(input.Frame, func() error {
		r, err := s.read(strings.Split(input.Frame, "/")[0])
		if err != nil {
			return err
		}
		input.Frame = r.Site.ID + "/" + r.Site.ModelRevision
		if old := r.LocalizationGeometry; old != nil && old.Frame == input.Frame && slices.Equal(old.Architecture, input.Architecture) && slices.Equal(old.Furniture, input.Furniture) {
			return nil
		}
		if err := twinrobot.ValidateSearchGeometry(input); err != nil {
			return err
		}
		r.LocalizationGeometry = &RoomLocalizationGeometry{Frame: input.Frame, Architecture: append([]float64(nil), input.Architecture...), Furniture: append([]float64(nil), input.Furniture...)}
		r.HasLocalizationGeometry = true
		r.Revision++
		return s.write(r)
	})
}

func (s *Server) localizationGeometry(w http.ResponseWriter, r *http.Request) {
	var input twinrobot.SiteFusionRequest
	if !decode(w, r, &input, 4<<20) {
		return
	}
	if !strings.EqualFold(strings.Split(input.Frame, "/")[0], r.PathValue("id")) {
		respond(w, nil, errors.New("room geometry frame does not match the requested room"))
		return
	}
	if err := s.Store.SaveLocalizationGeometry(input); err != nil {
		respond(w, nil, err)
		return
	}
	room, err := s.Store.Frame(input.Frame)
	respond(w, map[string]any{"frame": input.Frame, "hasLocalizationGeometry": true, "revision": room.Revision}, err)
}
