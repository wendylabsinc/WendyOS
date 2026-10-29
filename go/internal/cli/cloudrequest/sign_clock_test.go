package cloudrequest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// jwsLifetime returns the JWS payload's exp claim minus its iat claim.
func jwsLifetime(t *testing.T, jws []byte, iat, exp string) int64 {
	t.Helper()
	parts := strings.Split(string(jws), ".")
	if len(parts) != 3 {
		t.Fatalf("not a compact JWS: %q", jws)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]json.RawMessage
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	var issued, expires int64
	if json.Unmarshal(claims[iat], &issued) != nil || json.Unmarshal(claims[exp], &expires) != nil {
		t.Fatalf("missing %s or %s in %s", iat, exp, payload)
	}
	return expires - issued
}

// Cloud admits a descriptor whose iat is within ±60 s of its clock and whose
// expiry has not passed, so a 60 s lifetime tolerates an operator clock up to
// a minute slow, as the console's does. The DeleteAsset descriptor is checked
// again by the agent's reset verifier, which caps it at 30 s, and the PKI
// management request keeps its 30 s too (WDY-3939 #11).
func TestSignatureLifetimes(t *testing.T) {
	auth, _, _ := testAuth(t)
	signer, err := newSigner(auth)
	if err != nil {
		t.Fatal(err)
	}
	enroll, err := signer.signRequest(cloudpbv2.DeviceEnrollmentService_EnrollDevice_FullMethodName, &cloudpbv2.EnrollDeviceRequest{DeviceId: "dev-1"}, false, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if got := jwsLifetime(t, enroll.GetSignature(), "iat", "expiry"); got != 60 {
		t.Fatalf("EnrollDevice descriptor lifetime = %ds, want 60s", got)
	}
	device := uuid.NewString()
	deletion, err := signer.signRequest(cloudpbv2.AssetService_DeleteAsset_FullMethodName, &cloudpbv2.DeleteAssetRequest{Id: uuid.NewString(), ExpectedDeviceId: proto.String(device)}, false, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if got := jwsLifetime(t, deletion.GetSignature(), "iat", "expiry"); got != 30 {
		t.Fatalf("DeleteAsset descriptor lifetime = %ds, want 30s", got)
	}
	if got := jwsLifetime(t, deletion.GetPkiManagementRequest(), "iat", "exp"); got != 30 {
		t.Fatalf("PKI management request lifetime = %ds, want 30s", got)
	}
}

// Cloud refuses a descriptor outside its validity window with ErrorInfo
// reason "expired". Signing again with x5c under the same clock cannot help,
// so the refusal is final and says to check the clock.
func TestInvokeExpiredSignatureNamesTheClock(t *testing.T) {
	const (
		enroll   = cloudpbv2.DeviceEnrollmentService_EnrollDevice_FullMethodName
		register = cloudpbv2.OperatorSessionService_RegisterOperatorLeaf_FullMethodName
	)
	st, err := status.New(codes.PermissionDenied, "the operator request signature was rejected").WithDetails(&errdetails.ErrorInfo{Reason: "expired", Domain: "wendy.sh"})
	if err != nil {
		t.Fatal(err)
	}
	auth, _, _ := testAuth(t)
	conn := &fakeCloud{t: t, refuse: []error{st.Err()}}
	err = Invoke(context.Background(), conn, auth, enroll, &cloudpbv2.EnrollDeviceRequest{DeviceId: "dev-1"}, &cloudpbv2.EnrollDeviceResponse{})
	if err == nil || !strings.Contains(err.Error(), "clock") {
		t.Fatalf("Invoke err = %v, want a refusal that names the clock", err)
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("status code = %v, want PermissionDenied kept", status.Code(err))
	}
	if want := []string{register + " x5c", enroll + " kid"}; strings.Join(conn.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(conn.calls, "\n"), strings.Join(want, "\n"))
	}
}
