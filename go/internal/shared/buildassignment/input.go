// Package buildassignment binds a delivery grant to the complete build request.
package buildassignment

import (
	"crypto/sha256"
	"encoding/base64"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/protobuf/proto"
)

// InputSHA256 excludes only the grants themselves, avoiding a circular hash.
// Deterministic protobuf encoding also binds unknown fields and target order.
func InputSHA256(spec *agentpbv2.BuildSpec) (string, error) {
	copy := proto.Clone(spec).(*agentpbv2.BuildSpec)
	targets := append([]*agentpbv2.PushTarget{copy.GetPushTarget()}, copy.GetPushTargets()...)
	for _, target := range targets {
		if target != nil {
			target.DeliveryAuthorizationJws = ""
			target.DeliverySession = ""
		}
	}
	b, err := (proto.MarshalOptions{Deterministic: true}).Marshal(copy)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}
