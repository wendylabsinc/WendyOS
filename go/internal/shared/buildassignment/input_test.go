package buildassignment

import (
	"testing"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/protobuf/proto"
)

func TestInputBindingIncludesBuildAndTargetsButExcludesGrants(t *testing.T) {
	spec := &agentpbv2.BuildSpec{AppId: "app", Platform: "linux/arm64", Context: &agentpbv2.ChunkManifest{ChunkHashes: [][]byte{make([]byte, 32)}}, PushTarget: &agentpbv2.PushTarget{AssetUuid: "target", Repository: "app:latest", RegistryPort: 5000}, Definition: &agentpbv2.BuildSpec_DockerfileBuild{DockerfileBuild: &agentpbv2.DockerfileBuild{Dockerfile: "Dockerfile", BuildArgs: map[string]string{"version": "one"}}}}
	want, err := InputSHA256(spec)
	if err != nil {
		t.Fatal(err)
	}
	copy := proto.Clone(spec).(*agentpbv2.BuildSpec)
	copy.PushTarget.DeliveryAuthorizationJws = "signed grant"
	copy.PushTarget.DeliverySession = "admitted session"
	if got, _ := InputSHA256(copy); got != want {
		t.Fatal("grant makes input binding circular")
	}
	for _, mutate := range []func(*agentpbv2.BuildSpec){
		func(s *agentpbv2.BuildSpec) { s.Platform = "linux/amd64" },
		func(s *agentpbv2.BuildSpec) { s.Context.ChunkHashes[0][0] = 1 },
		func(s *agentpbv2.BuildSpec) { s.PushTarget.RegistryPort = 5555 },
		func(s *agentpbv2.BuildSpec) { s.PushTarget.Repository = "other:latest" },
		func(s *agentpbv2.BuildSpec) { s.GetDockerfileBuild().BuildArgs["version"] = "two" },
		func(s *agentpbv2.BuildSpec) { s.ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x01}) },
	} {
		copy := proto.Clone(spec).(*agentpbv2.BuildSpec)
		mutate(copy)
		if got, _ := InputSHA256(copy); got == want {
			t.Fatal("changed assignment retains binding")
		}
	}
}
