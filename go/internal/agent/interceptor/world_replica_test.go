package interceptor

import (
	"context"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	replicav1 "github.com/wendylabsinc/wendy/go/world/production/api/world/replica/v1"
	"google.golang.org/grpc/codes"
)

// Independent literal names pin the reviewed device capabilities. The existing
// interceptor helpers exercise unary and stream refusal before any handler is
// called. Their completed TLS state assumes the preceding native chain gate;
// these tests do not claim a TLS handshake or a commissioned World peer grant.
var worldReplicaRPCMethods = []string{
	"/wendy.world.replica.v1.WorldReplicaService/Inventory",
	"/wendy.world.replica.v1.WorldReplicaService/Fetch",
	"/wendy.world.replica.v1.WorldReplicaService/FetchEvidence",
	"/wendy.world.replica.v1.WorldReplicaService/PushLatest",
}

func TestWorldReplicaRPCExactMethods(t *testing.T) {
	if worldReplicaRPCMethods[0] != replicav1.WorldReplicaService_Inventory_FullMethodName || worldReplicaRPCMethods[1] != replicav1.WorldReplicaService_Fetch_FullMethodName || worldReplicaRPCMethods[2] != replicav1.WorldReplicaService_FetchEvidence_FullMethodName || worldReplicaRPCMethods[3] != replicav1.WorldReplicaService_PushLatest_FullMethodName {
		t.Fatal("generated replica method names changed from the reviewed device permissions")
	}
	scope := certs.Scope{TenantUUID: rpcTestTenant}
	device := rpcIdentityLeaf(t, "", "spiffe://wendy.sh/tenant/"+rpcTestTenant+"/device/22222222-2222-4222-8222-222222222222")
	ctx := rpcIdentityContext(device)
	for _, method := range worldReplicaRPCMethods {
		t.Run(method, func(t *testing.T) {
			assertRPCDecision(t, ctx, method, scope, codes.OK)
			t.Run("no authenticated transport", func(t *testing.T) {
				assertRPCDecision(t, context.Background(), method, scope, codes.Unauthenticated)
			})
			for _, lookalike := range []string{
				"prefix" + method,
				"/prefix" + method,
				method[1:],
				method + "Extra",
				method + "/Other",
				method + " ",
				method + "\x00",
			} {
				t.Run(lookalike, func(t *testing.T) {
					assertRPCDecision(t, ctx, lookalike, scope, codes.PermissionDenied)
				})
			}
		})
	}
	for _, method := range []string{
		"/wendy.world.replica.v2.WorldReplicaService/Inventory",
		"/wendy.world.replica.v1.WorldReplicaServiceExtra/Fetch",
		"/wendy.world.replica.v1.WorldReplicaService/inventory",
		"/wendy.world.replica.v1.WorldReplicaService/Publish",
		"/wendy.world.replica.v1.WorldReplicaService/Stop",
		"/wendy.world.app.v1.WorldAppService/Publish",
		"/wendy.world.app.v1.WorldAppService/PublishStream",
		"/wendy.world.app.v1.WorldAppService/Query",
		"/wendy.world.app.v1.WorldAppService/PutEvidence",
		"/wendy.world.app.v1.WorldAppService/GetEvidence",
		"/wendy.world.app.v1.WorldAppService/Clock",
		"/wendy.world.app.v1.WorldAppService/PhysicalClock",
		"/wendy.world.app.v1.WorldAppService/GetRecordProof",
		"/wendy.world.app.v1.WorldAppService/GetSourceAuthority",
		"/wendy.agent.services.v1.WendyShellService/HostShell",
		"/wendy.agent.services.v1.WendyContainerService/RunContainer",
	} {
		t.Run(method, func(t *testing.T) {
			assertRPCDecision(t, ctx, method, scope, codes.PermissionDenied)
		})
	}
}

func TestWorldReplicaRPCRequiresMatchingDeviceTenantInEveryMode(t *testing.T) {
	scope := certs.Scope{TenantUUID: rpcTestTenant}
	foreign := rpcIdentityContext(rpcIdentityLeaf(t, "wendy/user/alice", "spiffe://wendy.sh/tenant/33333333-3333-4333-8333-333333333333/device/44444444-4444-4444-8444-444444444444"))
	local := rpcIdentityContext(rpcIdentityLeaf(t, "", "spiffe://wendy.sh/tenant/"+rpcTestTenant+"/device/22222222-2222-4222-8222-222222222222"))
	for _, mode := range []OrgMode{OrgModeStrict, OrgModeGrace, OrgModeOff} {
		t.Run(mode.String(), func(t *testing.T) {
			for _, method := range worldReplicaRPCMethods {
				t.Run(method, func(t *testing.T) {
					t.Run("matching", func(t *testing.T) {
						assertRPCDecisionInMode(t, local, method, scope, mode, codes.OK)
					})
					t.Run("foreign", func(t *testing.T) {
						assertRPCDecisionInMode(t, foreign, method, scope, mode, codes.PermissionDenied)
					})
					t.Run("unknown server tenant", func(t *testing.T) {
						assertRPCDecisionInMode(t, local, method, certs.Scope{}, mode, codes.PermissionDenied)
					})
				})
			}
		})
	}
}

func TestWorldReplicaRPCDoesNotChangeOperatorCertificateGate(t *testing.T) {
	scope := certs.Scope{TenantUUID: rpcTestTenant}
	device := rpcIdentityLeaf(t, "wendy/user/alice", "spiffe://wendy.sh/tenant/"+rpcTestTenant+"/device/22222222-2222-4222-8222-222222222222")
	operator := rpcIdentityLeaf(t, "", "spiffe://wendy.sh/tenant/"+rpcTestTenant+"/operator/alice")
	foreign := rpcIdentityLeaf(t, "", "spiffe://wendy.sh/tenant/33333333-3333-4333-8333-333333333333/operator/alice")
	for _, mode := range []OrgMode{OrgModeStrict, OrgModeGrace, OrgModeOff} {
		t.Run(mode.String(), func(t *testing.T) {
			if err := CheckOperatorCertificate(device, scope, mode); err == nil {
				t.Fatal("replica-capable device acquired operator-only access")
			}
			if err := CheckOperatorCertificate(operator, scope, mode); err != nil {
				t.Fatalf("matching operator lost its existing management gate: %v", err)
			}
			if err := CheckOperatorCertificate(foreign, scope, mode); err == nil {
				t.Fatal("foreign operator acquired management access")
			}
			// Existing operator RPC admission is unchanged. Replica handlers
			// separately require a device and explicit current World permission.
			for _, method := range worldReplicaRPCMethods {
				t.Run(method, func(t *testing.T) {
					assertRPCDecisionInMode(t, rpcIdentityContext(operator), method, scope, mode, codes.OK)
				})
			}
		})
	}
}
