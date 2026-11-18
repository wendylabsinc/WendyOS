package mcusource

import (
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"google.golang.org/protobuf/proto"
)

// ValidGenericDescriptor bounds opaque fixed-size samples before subscription.
// No schema allowlist: interpretation belongs to the consuming application.
// A zero rate is allowed for event-driven sources; zero/unspecified sample size
// needs a shared protocol contract before it can be forwarded safely.
func ValidGenericDescriptor(d *sensorlinkpb.SensorDescriptor) bool {
	f := d.GetSensor()
	return f != nil && len(f.GetSchema()) > 0 && len(f.GetSchema()) <= 1024 &&
		len(d.GetName()) <= 256 && proto.Size(d) <= 2048 && f.GetSampleBytes() > 0 && f.GetSampleBytes() <= 65536
}
