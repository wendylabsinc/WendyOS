package derivedreader

import (
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

// Interpretation comes only from a wrapper's fixed domain decoder. It
// describes an original publication and supplies no current authority or time.
type Interpretation[T any] struct {
	Payload            T
	ID, RunID          string
	Frame              world.FrameRef
	SampleValidUntil   time.Time
	PhysicalValidUntil time.Time
}

type Decoder[T any] func(serviceorigin.PublishRequest) (Interpretation[T], error)
