//go:build linux

package containerd

import (
	"bytes"
	"context"
	"crypto/hpke"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/typeurl/v2"
	"google.golang.org/protobuf/proto"

	"github.com/wendylabsinc/wendy/go/internal/agent/secretmount"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/registrycrypto"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

func runtimeSecretFixture(t *testing.T) (*cloudpbv2.DeviceAssignment, string, string, string) {
	t.Helper()
	raw, err := os.ReadFile("../services/testdata/app-secrets-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Seed    string `json:"device_kem_ikm_hex"`
		Vectors []struct {
			Assignment string                         `json:"assignment_b64"`
			Expected   []struct{ Name, Value string } `json:"expected_secrets"`
		} `json:"vectors"`
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	v := fixture.Vectors[1]
	wire, _ := base64.StdEncoding.DecodeString(v.Assignment)
	a := &cloudpbv2.DeviceAssignment{}
	if err = proto.Unmarshal(wire, a); err != nil {
		t.Fatal(err)
	}
	seed, _ := hex.DecodeString(fixture.Seed)
	key, err := hpke.MLKEM768X25519().DeriveKeyPair(seed)
	if err != nil {
		t.Fatal(err)
	}
	private, err := key.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	record, _ := json.Marshal(map[string]any{"principal": a.Device, "privateKey": private})
	config := t.TempDir()
	if err = os.WriteFile(filepath.Join(config, "device-encryption-key.json"), record, 0600); err != nil {
		t.Fatal(err)
	}
	return a, v.Expected[0].Name, v.Expected[0].Value, config
}

// Produce a fresh encapsulation of the same fixture key, as on key rotation.
func rewrapRuntimeSecret(t *testing.T, a *cloudpbv2.DeviceAssignment, config string) *cloudpbv2.DeviceAssignment {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(config, "device-encryption-key.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record struct{ PrivateKey []byte }
	if err = json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	key, err := hpke.MLKEM768X25519().NewPrivateKey(record.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	b := proto.Clone(a).(*cloudpbv2.DeviceAssignment)
	secret := b.Secrets[0]
	v, w := secret.Value, secret.DeviceWrap
	wrap := registrycrypto.Wrap{OrganizationID: b.OrganizationId, Scope: registrycrypto.ScopeSecret, ScopeID: b.AppId + "/" + v.Name + "/" + strconv.Itoa(int(v.Version)), KeyID: v.SecretKeyId, RecipientKind: 1, Recipient: b.Device, RecipientKeyID: w.RecipientKeyId}
	plaintext, err := wrap.Open(key, w.Enc, w.Ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(plaintext)
	w.Enc, w.Ciphertext, err = wrap.Seal(key.PublicKey().Bytes(), plaintext)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestIntegrationAppSecretsLifecycle(t *testing.T) {
	c, ctx := newIntegrationClient(t)
	if snapshotter := os.Getenv("WENDY_SECRETS_SNAPSHOTTER"); snapshotter != "" {
		c.snapshotter = snapshotter
	}
	createReplaceTestImage(t, c, ctx)
	a, name, want, config := runtimeSecretFixture(t)
	c.SetAppSecretOpener(services.NewAppSecretOpener(config))
	ctx = services.WithReleaseSecrets(ctx, a)
	cfg := &appconfig.AppConfig{AppID: a.AppId}
	req := &agentpb.CreateContainerRequest{ImageName: replaceTestImage, AppName: a.AppId}
	if err := c.CreateContainer(ctx, req, cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.DeleteContainer(context.WithoutCancel(ctx), a.AppId, false) })
	ctr, err := c.client.LoadContainer(ctx, a.AppId)
	if err != nil {
		t.Fatal(err)
	}
	// The running process is non-root; mount ownership follows its resolved UID.
	spec, err := ctr.Spec(ctx)
	if err != nil {
		t.Fatal(err)
	}
	spec.Process.User.UID = 1234
	spec.Process.User.GID = 1234
	spec.Process.Args = []string{"/bin/sh", "-c", "/bin/busybox cat /run/secrets/" + name + " || exit 1; if echo changed > /run/secrets/" + name + " 2>/dev/null; then exit 2; fi; printf '\\nreadonly\\n'; trap 'exit 0' TERM; while :; do /bin/busybox sleep 3600 & wait $!; done"}
	if err = ctr.Update(ctx, func(_ context.Context, _ *containerd.Client, info *containers.Container) error {
		info.Spec, err = typeurl.MarshalAny(spec)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	start := func() string {
		t.Helper()
		out, err := c.StartContainer(ctx, a.AppId, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		// The app's non-root entrypoint reads the file and attempts a write.
		var stdout bytes.Buffer
		timeout := time.NewTimer(5 * time.Second)
		defer timeout.Stop()
		expected := want + "\nreadonly\n"
		for stdout.Len() < len(expected) {
			select {
			case chunk, ok := <-out:
				if !ok || chunk.Done {
					t.Fatal("app exited before reading its secret")
				}
				stdout.Write(chunk.Stdout)
			case <-timeout.C:
				t.Fatal("app did not read its secret")
			}
		}
		go func() {
			for range out {
			}
		}()
		if stdout.String() != expected {
			t.Fatal("app secret bytes changed or were writable")
		}
		spec, err := ctr.Spec(ctx)
		if err != nil {
			t.Fatal(err)
		}
		mount := spec.Annotations[secretMountAnnotation]
		if mount == "" {
			t.Fatal("secret mount missing")
		}
		info, err := ctr.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		serialized, _ := json.Marshal(info)
		if bytes.Contains(serialized, []byte("Zml4dHVyZQ==")) {
			t.Fatal("plaintext persisted in container metadata")
		}
		for _, entry := range spec.Process.Env {
			if strings.HasPrefix(entry, name+"=") {
				t.Fatal("secret injected into environment")
			}
		}
		return mount
	}
	first := start()
	rewrapped := rewrapRuntimeSecret(t, a, config)
	if err = c.UpdateAppSecretWraps(ctx, rewrapped); err != nil {
		t.Fatal(err)
	}
	rewrappedInfo, err := ctr.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	persisted := &cloudpbv2.DeviceAssignment{}
	if err = proto.Unmarshal(rewrappedInfo.Extensions[secretExtension].GetValue(), persisted); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(persisted.Secrets[0], rewrapped.Secrets[0]) {
		t.Fatal("rewrap was not persisted")
	}
	activeSpec, err := ctr.Spec(ctx)
	if err != nil || activeSpec.Annotations[secretMountAnnotation] != first {
		t.Fatal("rewrap replaced active mount", err)
	}

	orphan, err := secretmount.Prepare(map[string][]byte{"ORPHAN": []byte("fixture")}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = secretmount.Cleanup(orphan) })
	if err = c.recoverAppSecretMounts(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("startup retained orphan mount")
	}
	if _, err = os.Stat(first); err != nil {
		t.Fatal("startup removed a running app's secrets", err)
	}

	if err = c.StopContainer(ctx, a.AppId); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(first); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stop retained secret mount")
	}
	// Rebuild the runtime client using only persisted container metadata.
	c = &Client{client: c.client, logger: c.logger, namespace: c.namespace, snapshotter: c.snapshotter}
	c.SetAppSecretOpener(services.NewAppSecretOpener(config))
	second := start()
	if first == second {
		t.Fatal("restart reused secret mount")
	}
	// A late old task cleanup cannot remove a later start's mount.
	c.cleanupAppSecretMount(first)
	if _, err = os.Stat(second); err != nil {
		t.Fatal("old cleanup removed replacement mount", err)
	}
	if err = c.StopContainer(ctx, a.AppId); err != nil {
		t.Fatal(err)
	}
	c.SetAppSecretOpener(func(*cloudpbv2.DeviceAssignment) (map[string][]byte, error) {
		return nil, errors.New("fixture decrypt failure")
	})
	if _, err = c.StartContainer(ctx, a.AppId, "", nil); err == nil {
		t.Fatal("started without decrypting secrets")
	}
	c.SetAppSecretOpener(services.NewAppSecretOpener(config))
	// A runtime failure after mount preparation must release its plaintext.
	failedSpec, err := ctr.Spec(ctx)
	if err != nil {
		t.Fatal(err)
	}
	savedArgs := failedSpec.Process.Args
	failedSpec.Process.Args = []string{"/does-not-exist"}
	if err = ctr.Update(ctx, withUpdatedContainerSpec(failedSpec)); err != nil {
		t.Fatal(err)
	}
	if _, err = c.StartContainer(ctx, a.AppId, "", nil); err == nil {
		t.Fatal("started invalid entrypoint")
	}
	failedSpec, err = ctr.Spec(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(failedSpec.Annotations[secretMountAnnotation]); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed task creation retained secrets")
	}
	failedSpec.Process.Args = savedArgs
	if err = ctr.Update(ctx, withUpdatedContainerSpec(failedSpec)); err != nil {
		t.Fatal(err)
	}
	third := start()
	// A revision that removes every secret also removes the persisted envelope.
	plainCtx := c.withNamespace(context.Background())
	if err = c.CreateContainer(plainCtx, req, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(third); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("replacement retained old secrets")
	}
	ctr, err = c.client.LoadContainer(ctx, a.AppId)
	if err != nil {
		t.Fatal(err)
	}
	info, err := ctr.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Extensions[secretExtension] != nil || info.Labels[secretContentLabel] != "" {
		t.Fatal("secret removal retained restart material")
	}
	// Install the sealed revision again and verify explicit removal while running.
	if err = c.CreateContainer(ctx, req, cfg); err != nil {
		t.Fatal(err)
	}
	ctr, err = c.client.LoadContainer(ctx, a.AppId)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := ctr.Spec(ctx)
	if err != nil {
		t.Fatal(err)
	}
	restored.Process.Args = savedArgs
	restored.Process.User.UID, restored.Process.User.GID = 1234, 1234
	if err = ctr.Update(ctx, withUpdatedContainerSpec(restored)); err != nil {
		t.Fatal(err)
	}
	third = start()
	if err = c.DeleteContainer(ctx, a.AppId, false); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(third); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("removal retained secret mount")
	}
}
