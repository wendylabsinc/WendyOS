package twin

import (
	"github.com/wendylabsinc/wendy/go/internal/twinrobot"
	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
)

// robotControl separates the HTTP/job lifecycle from the live transport.
// Production uses the same tested Go2 engine for every operation.
type robotControl interface {
	Snapshot() twinrobot.SiteRobotSnapshot
	Status() twinrobot.SiteRobotSnapshot
	NextVideo() ([]byte, error)
	Close()
	ReleaseOperator()
	Stop(uint64)
	StartRoomFromOdometry(uint64, string) error
	Align(uint64, string, float64, float64, float64, float64) error
	Destination(uint64, string, float64, float64, bool) error
	Look(uint64, int) error
	SetArrivalTiming(uint64, bool) error
	SetUnderBodyAllowance(uint64, bool) error
	SetAllowUnobservedFloor(uint64, bool) error
	Localize(uint64, string, []sitenav.Triangle) ([]twinrobot.SiteRobotCandidate, error)
	ConfirmLocalization(uint64, uint64, string, int) error
	FuseLocalization(uint64, twinrobot.SiteFusionRequest) (twinrobot.SiteFusionReport, error)
	MappingSample() (string, uint64, []sitenav.Vertex)
}
