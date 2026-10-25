package cloudenroll

import (
	"context"

	"github.com/wendylabsinc/wendy-go/enrollment"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"google.golang.org/grpc"
)

// CheckAgentEnrollment checks readiness through the shared Core policy.
func CheckAgentEnrollment(ctx context.Context, conn grpc.ClientConnInterface) error {
	return enrollment.CheckAgentEnrollment(ctx, cloudrequest.EnrollmentTransport(conn))
}
