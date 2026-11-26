package containerd

import (
	"context"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/core/containers"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/protobuf/proto"

	localoci "github.com/wendylabsinc/wendy/go/internal/agent/oci"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

func TestSecretCreateStoresOnlyEncryptedEnvelope(t *testing.T) {
	a := &cloudpbv2.DeviceAssignment{AppId: "app", Secrets: []*cloudpbv2.DeviceSecret{{Value: &cloudpbv2.SealedSecret{Name: "TOKEN", Ciphertext: []byte("ciphertext")}}}}
	ctx := services.WithReleaseSecrets(context.Background(), a)
	labels := map[string]string{}
	option, err := secretCreateOptions(ctx, "app", localoci.DefaultSpec("rootfs", []string{"sh"}), labels)
	if err != nil {
		t.Fatal(err)
	}
	var info containers.Container
	if err = option(ctx, nil, &info); err != nil {
		t.Fatal(err)
	}
	decoded := &cloudpbv2.DeviceAssignment{}
	if err = proto.Unmarshal(info.Extensions[secretExtension].GetValue(), decoded); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(decoded.Secrets[0], a.Secrets[0]) {
		t.Fatal("lost encrypted secret")
	}
	if labels[secretContentLabel] != services.SecretContentID(a) {
		t.Fatal("missing revision identity")
	}
	for _, v := range labels {
		if strings.Contains(v, "ciphertext") {
			t.Fatal("secret payload stored as label")
		}
	}
}

func TestSecretMountConflicts(t *testing.T) {
	a := &cloudpbv2.DeviceAssignment{AppId: "app", Secrets: []*cloudpbv2.DeviceSecret{{Value: &cloudpbv2.SealedSecret{Name: "TOKEN"}}}}
	for _, dest := range []string{"/run/secrets", "/run/secrets/TOKEN", "/run/secrets/../secrets/nested"} {
		spec := localoci.DefaultSpec("rootfs", []string{"sh"})
		spec.Mounts = append(spec.Mounts, localoci.Mount{Destination: dest})
		_, err := secretCreateOptions(services.WithReleaseSecrets(context.Background(), a), "app", spec, map[string]string{})
		if err == nil {
			t.Fatalf("accepted conflicting mount %q", dest)
		}
	}
	if conflictsWithSecrets("/run/secrets-other") {
		t.Fatal("unrelated mount conflicts")
	}
}

func TestSecretOwnerMapping(t *testing.T) {
	maps := []specs.LinuxIDMapping{{ContainerID: 0, HostID: 100000, Size: 65536}}
	if got, err := hostID(1234, maps); err != nil || got != 101234 {
		t.Fatalf("mapped owner = %d, %v", got, err)
	}
	if _, err := hostID(65536, maps); err == nil {
		t.Fatal("unmapped UID accepted")
	}
	if _, err := hostID(5, []specs.LinuxIDMapping{{ContainerID: 0, HostID: 1<<32 - 2, Size: 10}}); err == nil {
		t.Fatal("overflow accepted")
	}
}
