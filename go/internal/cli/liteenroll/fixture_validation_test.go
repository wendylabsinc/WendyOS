package liteenroll

import (
	litepb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"google.golang.org/protobuf/proto"
)

// Exercise Core's generic PEM/CA checks with all supported algorithms enabled.
func validateTrustBundle(data []byte) error {
	return validateTrustBundleForProfile(data, TrustProfileFromChallenge(&litepb.WendyComEnrollmentChallenge{MldsaVerificationSupported: proto.Bool(true)}))
}
