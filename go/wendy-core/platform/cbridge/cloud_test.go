package cbridge_test

import (
	"bytes"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/wendy-core/core/cloud"
	pb "github.com/wendylabsinc/WendyOS/go/wendy-core/internal/bindingpb"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform/cbridge/internal/testhost"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

const cloudTenant = "12345678-1234-1234-1234-123456789abc"

func cloudRequest() *pb.CloudDeviceListRequest {
	return &pb.CloudDeviceListRequest{Authority: "cloud.example:443", OrganizationId: cloudTenant,
		AccessToken: "access-token", Filter: "robot", OnlineOnly: true}
}
func marshal(t *testing.T, message proto.Message) []byte {
	t.Helper()
	wire, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	return wire
}
func cloudHost(t *testing.T, mode int) *testhost.Host {
	t.Helper()
	fake, _ := host(t, mode)
	// A structurally valid ML-DSA SPKI. The fake signs without cryptography;
	// production key/certificate verification is explicitly the host's duty.
	key, err := asn1.Marshal(struct {
		Algorithm struct{ OID asn1.ObjectIdentifier }
		PublicKey asn1.BitString
	}{Algorithm: struct{ OID asn1.ObjectIdentifier }{asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 3, 18}},
		PublicKey: asn1.BitString{Bytes: make([]byte, 1952), BitLength: 1952 * 8}})
	if err != nil {
		t.Fatal(err)
	}
	response := func(id string) []byte {
		asset := &cloud.Asset{Id: id, OrganizationId: cloudTenant, Name: "robot", IsComputeDevice: true, DisplayName: proto.String("Robot 🐕"), Tags: []string{"test"}}
		asset.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 500, protowire.VarintType), 42))
		wire := protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), marshal(t, asset))
		return protowire.AppendVarint(protowire.AppendTag(wire, 2, protowire.VarintType), 2)
	}
	fake.Cloud(key, response("first"), response("second"))
	return fake
}
func createCloud(t *testing.T, fake *testhost.Host, request *pb.CloudDeviceListRequest) *testhost.Completion {
	t.Helper()
	c := testhost.CloudCreate(fake.ID(), marshal(t, request))
	if c.Stat(4) != 1 {
		t.Fatal("Create returned NULL")
	}
	t.Cleanup(c.Free)
	return c
}
func completed(t *testing.T, c *testhost.Completion) {
	t.Helper()
	eventually(t, func() bool { return c.Stat(0) != 0 })
	if c.Stat(0) != 1 {
		t.Fatal("completion called more than once")
	}
}

func TestCloudDeviceListAcrossC(t *testing.T) {
	fake := cloudHost(t, 7)
	request := cloudRequest()
	request.DeadlineUnixMillis = time.Now().Add(time.Minute).UnixMilli()
	c := createCloud(t, fake, request)
	// No callback or host call is allowed while the host installs its handle.
	if c.Stat(0) != 0 || fake.Stat(1) != 0 {
		t.Fatal("operation started during Create")
	}
	c.ReleaseInCallback() // Completion can cancel and release without deadlock.
	c.Start()
	completed(t, c)
	if c.Stat(1) != 0 || c.Stat(2) != 1 || c.Stat(3) != 0 {
		t.Fatalf("wrong result: %s", c.Result(1))
	}
	var result pb.CloudDeviceListResponse
	if err := proto.Unmarshal(c.Result(0), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Assets) != 2 {
		t.Fatalf("not a complete inventory: %v", &result)
	}
	for i, wire := range result.Assets {
		var asset cloud.Asset
		if err := proto.Unmarshal(wire, &asset); err != nil {
			t.Fatal(err)
		}
		if asset.GetId() != []string{"first", "second"}[i] || asset.GetOrganizationId() != cloudTenant || asset.GetDisplayName() != "Robot 🐕" || len(asset.ProtoReflect().GetUnknown()) == 0 {
			t.Fatalf("asset lost fields: %v", &asset)
		}
	}
	if fake.Stat(8) != 3 || fake.Stat(9) != 2 || fake.Stat(10) != 2 {
		t.Fatal("authentication did not run for both pages")
	}
	if fake.Stat(7) != uint64(request.DeadlineUnixMillis) {
		t.Fatal("host deadline lost")
	}
	var asset cloud.Asset
	descriptor := asset.ProtoReflect().Descriptor().ParentFile().Messages().ByName("ListAssetsRequest")
	for i := 0; i < 2; i++ {
		request := dynamicpb.NewMessage(descriptor)
		if err := proto.Unmarshal(fake.CloudView(i+1), request); err != nil {
			t.Fatal(err)
		}
		get := func(name string) protoreflect.Value {
			return request.Get(descriptor.Fields().ByName(protoreflect.Name(name)))
		}
		if get("organization_id").String() != cloudTenant || get("offset").Int() != int64(i) || get("limit").Int() != 200 || !get("is_compute_device").Bool() || !get("online_only").Bool() || get("filter").String() != "robot" {
			t.Fatalf("page selectors changed: %v", request)
		}
	}
	parts := strings.Split(string(fake.CloudView(0)), ".")
	if len(parts) != 3 {
		t.Fatal("no DPoP proof")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	if claims["htm"] != "POST" || claims["htu"] != "https://cloud.example:443/wendycloud.v2.AssetService/ListAssets" || claims["ath"] == nil || claims["jti"] == nil {
		t.Fatalf("DPoP lost token/endpoint binding: %s", payload)
	}
}

func TestCloudStatusAcrossC(t *testing.T) {
	for _, mode := range []int{8, 12} {
		t.Run(fmt.Sprintf("mode %d", mode), func(t *testing.T) {
			fake := cloudHost(t, mode)
			c := createCloud(t, fake, cloudRequest())
			c.Start()
			c.Start() // Must not execute the operation twice.
			completed(t, c)
			expected := uint64(7)
			if mode == 12 {
				expected = 123
			}
			if c.Stat(1) != expected || c.Stat(2) != 0 || c.Stat(3) != 1 || string(c.Result(1)) != "private remote diagnostic" {
				t.Fatalf("status or partial-result contract lost: %d, %s", c.Stat(1), c.Result(1))
			}
			var details pb.RPCErrorDetails
			if err := proto.Unmarshal(c.Result(2), &details); err != nil {
				t.Fatal(err)
			}
			if len(details.InitialMetadata) != 2 || len(details.TrailingMetadata) != 2 || details.InitialMetadata[0].Name != "trace-bin" || details.InitialMetadata[1].Name != "trace-bin" || details.TrailingMetadata[0].Name != "details-bin" || !bytes.Equal(details.TrailingMetadata[0].Value, []byte{0, 255, 0}) {
				t.Fatalf("lost status metadata: %v", &details)
			}
		})
	}
}

func TestCloudCancellationAcrossC(t *testing.T) {
	for _, mode := range []int{9, 10} {
		t.Run(fmt.Sprintf("mode %d", mode), func(t *testing.T) {
			fake := cloudHost(t, mode)
			c := createCloud(t, fake, cloudRequest())
			c.Start()
			eventually(t, func() bool { return mode == 9 && fake.Stat(10) == 1 || mode == 10 && fake.Stat(8) == 1 })
			call := fake.Stat(0)
			c.Cancel()
			c.Cancel()
			completed(t, c)
			eventually(t, func() bool { return fake.Stat(3) == 1 })
			if c.Stat(1) != 1 || c.Stat(2) != 0 {
				t.Fatalf("not cancellation: %s", c.Result(1))
			}
			testhost.Complete(call, 0)
			testhost.StaleComplete(call) // A late result must not even be read.
			if c.Stat(0) != 1 {
				t.Fatal("late completion delivered twice")
			}
		})
	}
}

func TestCloudHostReleaseAndDeadline(t *testing.T) {
	for _, release := range []bool{true, false} {
		t.Run(map[bool]string{true: "host release", false: "deadline"}[release], func(t *testing.T) {
			fake := cloudHost(t, 9)
			request := cloudRequest()
			if !release {
				request.DeadlineUnixMillis = time.Now().Add(time.Second).UnixMilli()
			}
			c := createCloud(t, fake, request)
			c.Start()
			eventually(t, func() bool { return fake.Stat(10) == 1 })
			if release {
				fake.Release()
			}
			completed(t, c)
			expected := uint64(4)
			if release {
				expected = 1
			}
			if c.Stat(1) != expected {
				t.Fatalf("status %d: %s", c.Stat(1), c.Result(1))
			}
			eventually(t, func() bool { return fake.Stat(3) == 1 })
		})
	}
}

func TestCloudLifecycleAndInvalidInput(t *testing.T) {
	if !testhost.InvalidCloudCreate() {
		t.Fatal("invalid views or NULL operations mishandled")
	}
	t.Run("release before start", func(t *testing.T) {
		fake := cloudHost(t, 7)
		c := createCloud(t, fake, cloudRequest())
		c.ReleaseOperation()
		if c.Stat(0) != 0 || fake.Stat(1) != 0 {
			t.Fatal("dormant operation executed")
		}
	})
	t.Run("cancel before start", func(t *testing.T) {
		fake := cloudHost(t, 7)
		c := createCloud(t, fake, cloudRequest())
		c.Cancel()
		if c.Stat(0) != 0 {
			t.Fatal("cancel bypassed activation gate")
		}
		c.Start()
		completed(t, c)
		if c.Stat(1) != 1 || fake.Stat(1) != 0 {
			t.Fatal("canceled operation performed host work")
		}
	})
	t.Run("release while running", func(t *testing.T) {
		fake := cloudHost(t, 9)
		c := createCloud(t, fake, cloudRequest())
		c.Start()
		eventually(t, func() bool { return fake.Stat(10) == 1 })
		c.ReleaseOperation()
		if fake.Stat(3) != 0 {
			t.Fatal("release canceled work")
		}
		fake.Release()
		completed(t, c)
	})
	t.Run("empty success", func(t *testing.T) {
		fake := cloudHost(t, 11)
		c := createCloud(t, fake, cloudRequest())
		c.Start()
		completed(t, c)
		if c.Stat(2) != 1 || c.Stat(3) != 0 || len(c.Result(0)) != 0 {
			t.Fatal("empty success confused with failure")
		}
	})
	for _, bad := range []string{"", "https://cloud.example", "user@cloud.example", "cloud.example/path", "cloud.example?", "cloud.example#"} {
		t.Run("authority "+bad, func(t *testing.T) {
			fake := cloudHost(t, 7)
			request := cloudRequest()
			request.Authority = bad
			c := createCloud(t, fake, request)
			c.Start()
			completed(t, c)
			if c.Stat(1) != 3 || fake.Stat(1) != 0 {
				t.Fatal("invalid authority reached host")
			}
		})
	}
	t.Run("malformed protobuf", func(t *testing.T) {
		fake := cloudHost(t, 7)
		c := testhost.CloudCreate(fake.ID(), []byte{255})
		defer c.Free()
		c.Start()
		completed(t, c)
		if c.Stat(1) != 3 || fake.Stat(1) != 0 {
			t.Fatal("invalid protobuf reached host")
		}
	})
}
