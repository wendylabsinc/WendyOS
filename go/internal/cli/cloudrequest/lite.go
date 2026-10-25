package cloudrequest

import (
	"context"
	"fmt"

	"github.com/wendylabsinc/wendy-go/cloud"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"google.golang.org/grpc/metadata"
)

// LiteEnrollmentSigner borrows the CLI's checked operator key for WendyGo Lite.
func LiteEnrollmentSigner(ctx context.Context, auth *config.AuthConfig) (*cloud.Signer, error) {
	s, err := newSigner(auth)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, fmt.Errorf("device enrollment requires an operator certificate")
	}
	shared, err := s.sdk(ctx)
	if err != nil {
		return nil, err
	}
	md, _ := metadata.FromOutgoingContext(ctx)
	return shared.WithMetadata(coreMetadata(md)), nil
}
