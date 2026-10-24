package cloud

import (
	"context"
	"fmt"
)

// EnrollmentChainP256 selects the PKI's P-256 device authority for constrained
// devices. The operator signature binds this choice alongside the device tier.
const EnrollmentChainP256 = "ECDSA_P256"

// EnrollmentRequest encodes PKI enrollment authority with the full operator
// chain. Enrollment orchestration stays in the host.
func (s *Signer) EnrollmentRequest(ctx context.Context, deviceID, deviceClass, chainAlgorithm string) ([]byte, error) {
	if deviceClass != "A" && deviceClass != "B" && deviceClass != "C" {
		return nil, fmt.Errorf("invalid device class %q", deviceClass)
	}
	if chainAlgorithm != "" && (deviceClass != "C" || chainAlgorithm != EnrollmentChainP256) {
		return nil, fmt.Errorf("P-256 certificate chains are supported only for Class C enrollment")
	}
	now := s.now().Unix()
	jti, err := randomUUID(ctx, s.entropy)
	if err != nil {
		return nil, err
	}
	claims := map[string]any{
		"tenant": s.tenantUUID, "device_id": deviceID, "device_class": deviceClass,
		"iat": now, "exp": now + 300, "jti": jti,
	}
	if chainAlgorithm != "" {
		claims["chain_algorithm"] = chainAlgorithm
	}
	payload, err := canonicalJSON(claims)
	if err != nil {
		return nil, err
	}
	// This artifact is carried in the protobuf body, not HTTP metadata. Keep
	// the full chain for PKI's enrollment signature verifier.
	jws, err := s.signPayload(ctx, payload, map[string]any{"x5c": s.x5c})
	return []byte(jws), err
}
