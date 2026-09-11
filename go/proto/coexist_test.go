// Package proto holds the check that the vendored cloud contracts can share a
// process. The generated code itself lives in gen/ and is never hand-edited.
package proto

import (
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	_ "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	_ "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// TestCloudV1AndV2Coexist fails if wendycloud.v1 and wendycloud.v2 cannot be
// linked into the same binary. The protobuf runtime panics at init when two
// generated packages claim the same proto file path, so vendoring v2 under a
// path that collides with v1 would take down every process that imports both —
// which the agent and CLI will do for the whole WDY-2824 migration window.
func TestCloudV1AndV2Coexist(t *testing.T) {
	for _, name := range []string{
		"wendycloud.v1.NotificationService",
		"wendycloud.v2.NotificationService",
		"wendycloud.v2.DeviceEnrollmentService",
	} {
		if _, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(name)); err != nil {
			t.Errorf("%s not registered: %v", name, err)
		}
	}
}
