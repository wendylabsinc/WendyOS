// Package ros2inspection defines the explicit host-inspection extension to the
// ROS2 gRPC service. App inspection remains the default; selecting a DDS domain
// alone never changes network namespaces.
package ros2inspection

import (
	"fmt"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

const (
	ScopeMetadata = "x-wendy-ros2-scope"
	HostScope     = "host"
	AppScope      = "app"
	FastRTPSRMW   = "rmw_fastrtps_cpp"
	HostDistro    = "humble"
)

// HostOptions deliberately has no user-supplied image or executable. The
// standalone inspector uses a fixed ROS image with its bundled FastRTPS middleware.
type HostOptions struct {
	DomainID int
}

func (o HostOptions) Validate() error {
	if o.DomainID < appconfig.ROS2DomainIDMin || o.DomainID > appconfig.ROS2DomainIDMax {
		return fmt.Errorf("domain ID %d out of range [%d,%d]", o.DomainID, appconfig.ROS2DomainIDMin, appconfig.ROS2DomainIDMax)
	}
	return nil
}
