package commands

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// validateServicesBeforeReplace asks the agent to check every service's create
// request before any service is replaced. A multi-service deploy creates its
// services one by one, so without the check a service the agent refuses
// leaves the services before it on the new version and the rest on the old
// one (WDY-3850). It reports every refusal at once. An agent that predates
// ValidateContainer answers Unimplemented, and the deploy goes on service by
// service as before.
func validateServicesBeforeReplace(ctx context.Context, svc agentpb.WendyContainerServiceClient, names []string, request func(name string) (*agentpb.CreateContainerRequest, error)) error {
	var refusals []error
	for _, name := range names {
		req, err := request(name)
		if err != nil {
			return err
		}
		if _, err := svc.ValidateContainer(ctx, req); err != nil {
			if status.Code(err) == codes.Unimplemented {
				return nil
			}
			refusals = append(refusals, fmt.Errorf("service %s: %s", name, status.Convert(err).Message()))
		}
	}
	if len(refusals) > 0 {
		return fmt.Errorf("the device would refuse these services, so none of the %d services was replaced:\n%w", len(names), errors.Join(refusals...))
	}
	return nil
}
