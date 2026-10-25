package liteenroll

import (
	wendy "github.com/wendylabsinc/wendy-go"
	sdk "github.com/wendylabsinc/wendy-go/lite/enrollment"
	sdkpb "github.com/wendylabsinc/wendy-go/lite/pb"
	litepb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"google.golang.org/protobuf/proto"
)

// TrustProfile retains the firmware-advertised verifier policy in WendyGo.
type TrustProfile struct{ profile sdk.TrustProfile }

func (p TrustProfile) sdk() sdk.TrustProfile {
	out := p.profile
	out.Inspector = wendy.GoLiteCertificateInspector{}
	return out
}
func TrustProfileFromChallenge(ch *litepb.WendyComEnrollmentChallenge) TrustProfile {
	converted := &sdkpb.WendyComEnrollmentChallenge{MldsaVerificationSupported: proto.Bool(ch.GetMldsaVerificationSupported())}
	return TrustProfile{profile: sdk.TrustProfileFromChallenge(converted, wendy.GoLiteCertificateInspector{})}
}
func (p TrustProfile) WithExistingDeviceIssuer() TrustProfile {
	p.profile = p.sdk().WithExistingDeviceIssuer()
	return p
}
func (p TrustProfile) chainAlgorithm() string { return p.sdk().ChainAlgorithm() }
func validateMbedTLSTrustBundle(data []byte) error {
	return validateTrustBundleForProfile(data, TrustProfile{})
}
func validateTrustBundleForProfile(data []byte, p TrustProfile) error {
	return sdk.ValidateTrustBundle(data, p.sdk())
}
