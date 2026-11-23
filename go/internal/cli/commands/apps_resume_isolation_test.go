package commands

// Isolated CLI signing tier. Uses the existing local gRPC service fixtures and
// a forwarding client barrier, never scripted prompt answers or live services.
import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

type appsResumeBarrier struct {
	grpc.ClientConnInterface
	beforeCurrent func()
	advanced      bool
}

func (b *appsResumeBarrier) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
	if method == pb.DeploymentService_GetDeployment_FullMethodName && !b.advanced {
		b.advanced = true
		b.beforeCurrent()
	}
	return b.ClientConnInterface.Invoke(ctx, method, args, reply, opts...)
}
func TestAppsOperatorIsolationResumeConfirmationRace(t *testing.T) {
	if os.Getenv("WENDY_E2E_APPS_ISOLATION") != "true" {
		t.Skip("explicit isolated fixture opt-in required")
	}
	op, server, _, id := deployedFixture(t)
	device := certs.DeviceSPIFFEURI(testOperatorTenant, vectorDevice)
	server.pending[id] = []*pb.PendingDeviceStatement{{Kind: pb.PendingDeviceStatement_KIND_ASSIGN, DeploymentId: id, Device: device, Revision: 1}}
	old := proto.Clone(server.deploys[id]).(*pb.Deployment)
	before := len(server.assigned)
	var review bytes.Buffer
	barrier := &appsResumeBarrier{ClientConnInterface: op.conn, beforeCurrent: func() {
		// --yes confirms the printed review in the actual scripted path. The next
		// GetDeployment is current-key loading, after that confirmation and before
		// signPending. Commit N+1 at exactly this boundary.
		if !strings.Contains(review.String(), "revision 1") {
			t.Fatal("barrier was reached before revision one was reviewed")
		}
		next := proto.Clone(old).(*pb.Deployment)
		next.Revision = 2
		next.ConfigJson = `{"LOG_LEVEL":"trace"}`
		server.mu.Lock()
		server.deploys[id] = next
		server.pending[id] = []*pb.PendingDeviceStatement{{Kind: pb.PendingDeviceStatement_KIND_ASSIGN, DeploymentId: id, Device: device, Revision: 2}}
		server.mu.Unlock()
	}}
	op.conn = barrier
	err := runResume(context.Background(), op, "sh.wendy.example", "line-3", changeFlags{yes: true}, &review, false)
	if err != nil && !strings.Contains(err.Error(), "revision") && !strings.Contains(err.Error(), "nothing was signed") && !strings.Contains(err.Error(), "changed") {
		t.Fatalf("unexpected fixture failure: %v", err)
	}
	if err == nil && len(server.assigned) != before+1 {
		t.Fatal("resume neither refused nor signed exactly the reviewed instruction")
	}
	if !barrier.advanced {
		t.Fatal("the signing boundary was not exercised")
	}
	if server.deploys[id].Revision != 2 {
		t.Fatal("newer stored revision changed")
	}
	// Refusing is acceptable; any signatures must still describe the confirmed
	// N content. Signing N+1 under N's review is the bug this case detects.
	for _, assignment := range server.assigned[before:] {
		if assignment.Revision != 1 || assignment.ConfigJson != old.ConfigJson || assignment.AppReleaseId != old.AppReleaseId {
			t.Fatal("resume signed unconfirmed newer content")
		}
	}
}
