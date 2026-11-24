package commands

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/trustedkeys"
	"github.com/wendylabsinc/wendy/go/internal/cli/closedtest"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudenroll"
	"github.com/wendylabsinc/wendy/go/internal/cli/localkey"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// `wendy device enroll` reaches a Closed device over this computer's local
// key, through the agent's real key listener and role table: the owner key
// enrolls; an operator key is refused before Cloud reserves the name.
func TestEnrollClosedDeviceOverLocalKey(t *testing.T) {
	for _, c := range []struct {
		role trustedkeys.Role
		ok   bool
	}{{trustedkeys.RoleOwner, true}, {trustedkeys.RoleOperator, false}} {
		t.Run(c.role.String(), func(t *testing.T) {
			setTempConfig(t, &config.Config{})
			setPinCache(t)
			forbidPlaintext(t)
			mine := useLocalKey(t)
			owner := mine
			if c.role != trustedkeys.RoleOwner {
				owner, _ = localkey.LoadOrCreate(t.TempDir() + "/" + localkey.FileName)
			}
			agent := &acmeProvisioningServer{}
			d := closedtest.Start(t, closedDir(t, owner), func(s *grpc.Server) {
				agentpbv2.RegisterWendyProvisioningServiceServer(s, agent)
				agentpbv2.RegisterWendyDeviceInfoServiceServer(s, agent)
			})
			if c.role != trustedkeys.RoleOwner {
				spki, _ := localkey.PublicKeyDER(mine)
				ownerFP, _ := localkey.Fingerprint(owner)
				if _, err := d.Keys.Add(spki, c.role, "", ownerFP, time.Now()); err != nil {
					t.Fatal(err)
				}
			}

			cloud := &oidcEnrollmentServer{response: &cloudpbv2.EnrollDeviceResponse{AssetId: "asset-uuid", CredentialKind: "eab", EabKeyId: "eab-id", EabHmacKey: strings.Repeat("ab", 32)}}
			cloudSrv := grpc.NewServer()
			cloudpbv2.RegisterDeviceEnrollmentServiceServer(cloudSrv, cloud)
			cloudpbv2.RegisterOperatorSessionServiceServer(cloudSrv, cloud)
			auth := oidcEnrollmentAuth(t)
			auth.CloudGRPC = serveTest(t, cloudSrv)

			conn, _, err := dialAgentLadderWithCerts(ladderCtx(t), newDialTarget("box", d.Addr), nil)
			if err != nil {
				t.Fatalf("ladder: %v", err)
			}
			defer conn.Close()
			if !conn.IsLocalOperator {
				t.Fatal("not dialed over the local key")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err = runEnrollDeviceIdentity(ctx, conn, auth, "box", 0)
			if c.ok {
				if err != nil || agent.req == nil {
					t.Fatalf("owner enroll: err=%v, device started=%v", err, agent.req != nil)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "owner key") {
				t.Fatalf("operator enroll: err=%v, want a refusal naming the owner role", err)
			}
			if agent.req != nil || cloud.req != nil {
				t.Fatalf("operator key got past the owner check: device started=%v, Cloud reserved the name=%v", agent.req != nil, cloud.req != nil)
			}
		})
	}
}

// A caller without a key (the keyless listener, keyless apps on) is refused
// before Cloud reserves the name; an Open device passes.
func TestEnrollCheckRunsOnKeylessConnections(t *testing.T) {
	owner, _ := localkey.LoadOrCreate(t.TempDir() + "/" + localkey.FileName)
	dir := closedDir(t, owner)
	keys, _ := trustedkeys.Open(dir + "/" + trustedkeys.FileName)
	if err := keys.SetKeylessApps(true, keys.List()[0].Fingerprint()); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, addr string
		refused    bool
	}{
		{"closed, keyless", closedtest.Start(t, dir).Addr, true},
		{"open", closedtest.Start(t, t.TempDir()).Addr, false},
	} {
		conn, err := grpc.NewClient(c.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err = cloudenroll.RequireClosedDeviceOwner(ctx, conn)
		cancel()
		if c.refused != (err != nil) || (c.refused && !strings.Contains(err.Error(), "owner key")) {
			t.Errorf("%s: err = %v, refused want %v", c.name, err, c.refused)
		}
	}
}
