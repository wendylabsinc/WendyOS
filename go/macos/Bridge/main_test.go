package main

import (
	"bytes"
	"errors"
	invite "github.com/wendylabsinc/wendy/go/proto/gen/cloudinvitepb"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"testing"
	"unsafe"

	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	"github.com/wendylabsinc/wendy/go/macos/backend"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func cstring(p unsafe.Pointer) string {
	if p == nil {
		return ""
	}
	var result []byte
	for i := 0; ; i++ {
		value := *(*byte)(unsafe.Add(p, i))
		if value == 0 {
			return string(result)
		}
		result = append(result, value)
	}
}

func TestTypedResultsAndDestructors(t *testing.T) {
	for i := 0; i < 100; i++ {
		d := devices([]commands.DesktopDevice{{ID: "device-1", Name: "Cámara", Transport: "Cloud", Selector: "cloud://fixture"}}, nil)
		if d.count != 1 || cstring(unsafe.Pointer(d.items.name)) != "Cámara" || d.items.transport != 2 {
			t.Fatal("device fields changed")
		}
		WDDevicesFree(d)
		a := apps([]*pb.AppContainer{{AppName: "fixture", HttpPort: 8080, RunningState: pb.AppRunningState_CRASH_LOOPING,
			Services: []*pb.ServiceEntry{{Name: "api", RunningState: pb.AppRunningState_RUNNING}}}}, nil)
		if a.count != 1 || a.items.http_port != 8080 || a.items.state != 2 || a.items.service_count != 1 || cstring(unsafe.Pointer(a.items.services.name)) != "api" {
			t.Fatal("nested app fields changed")
		}
		WDAppsFree(a)
		c := cameras([]*pb.VideoDevice{{Id: 0, Name: "Camera zero", Path: "/dev/video0"}}, nil)
		if c.count != 1 || c.items.id != 0 || cstring(unsafe.Pointer(c.items.path)) != "/dev/video0" {
			t.Fatal("camera zero lost")
		}
		WDCamerasFree(c)
		recovery := statusResult(&backend.IdentityRecoveryError{Err: errors.New("identity changed")})
		if recovery.can_forget_identity != 1 || cstring(unsafe.Pointer(recovery.message)) != "identity changed" {
			t.Fatal("missing recovery")
		}
		WDStatusFree(recovery)
	}
	WDDevicesFree(devices(nil, nil))
	WDAppsFree(apps(nil, nil))
	WDCamerasFree(cameras(nil, nil))
	WDStatusFree(statusResult(nil))
	WDDevicesFree(nil)
	WDAppsFree(nil)
	WDCamerasFree(nil)
	WDStatusFree(nil)
	WDConnectionFree(nil)
	WDVPNFree(nil)
	WDEventsFree(nil)
}

func TestRawEventBuffersAndStructuredLogs(t *testing.T) {
	raw := []byte{0, 255, 0, 1, 128}
	batch := events([]backend.Event{
		{Kind: backend.EventVideo, Session: 42, Stream: 123, Bytes: raw},
		{Kind: backend.EventLog, Session: 42, Stream: 124, Log: &backend.LogRecord{
			Timestamp: 1234567890123456789, Level: backend.LogError, Body: "first\n  frame", App: "fixture", Service: "api",
		}},
	})
	defer WDEventsFree(batch)
	items := unsafe.Slice(batch.items, int(batch.count))
	if items[0].kind != 6 || items[0].session != 42 || items[0].stream != 123 {
		t.Fatal("video envelope changed")
	}
	raw[1] = 0 // C memory owns a copy, not a pointer into the Go slice.
	if !bytes.Equal(unsafe.Slice((*byte)(unsafe.Pointer(items[0].bytes)), int(items[0].byte_count)), []byte{0, 255, 0, 1, 128}) {
		t.Fatal("raw byte ownership changed")
	}
	if items[1].log.timestamp_ns != 1234567890123456789 || items[1].log.level != 5 || cstring(unsafe.Pointer(items[1].log.body)) != "first\n  frame" {
		t.Fatal("structured log changed")
	}
	WDEventsFree(events(nil))
}

func TestTypedEntryPointsRejectInvalidState(t *testing.T) {
	apps := WDApps()
	if apps.status.message == nil || apps.count != 0 {
		t.Fatal("apps without connection succeeded")
	}
	WDAppsFree(apps)
	vpn := WDStartVPN(nil, 0)
	if vpn.status.message == nil {
		t.Fatal("empty routes accepted")
	}
	WDVPNFree(vpn)
	connection := WDForgetIdentityAndReconnect(nil, WDBeginConnection())
	if connection.status.message == nil {
		t.Fatal("unpin without refusal accepted")
	}
	WDConnectionFree(connection)
	status := WDStartCamera(0, 1, 123)
	if status.message == nil {
		t.Fatal("camera without connection accepted")
	}
	WDStatusFree(status)
	WDEventsFree(WDPollEvents())
}

func TestResourceResultOwnsNestedValues(t *testing.T) {
	temp := 0.0
	seconds := int64(120)
	snapshot := &commands.DesktopResourceSnapshot{Host: &pb.HostStats{CpuCount: 4, MemTotalBytes: 4096, MemAvailableBytes: 1024, Gpus: []*pb.GpuStats{{Name: "GPU", TempC: &temp}}, ThermalZones: []*pb.ThermalZone{{Name: "CPU", TempC: 45}}, Battery: &pb.BatteryStats{Percent: 50, State: pb.BatteryState_BATTERY_STATE_CHARGING, SecondsRemaining: &seconds}}, Storage: &pb.DiskPartition{Mountpoint: "/data", UsedBytes: 1024, TotalBytes: 8192}, CPUPercent: 25, HasCPU: true, Rows: []commands.DesktopResourceRow{{ID: "app/api", App: "app", Name: "api", IsService: true, HasCPU: true, CPUPercent: 12, State: pb.AppRunningState_RUNNING}}}
	for i := 0; i < 100; i++ {
		result := resourceResult(snapshot, nil)
		if result.cpu.available != 1 || result.cpu.value != 25 || result.memory_used != 3072 || result.battery.has_estimate != 1 || result.battery.seconds_remaining != 120 {
			t.Fatal("host fields lost")
		}
		if result.gpus.temperature.available != 1 || result.gpus.power.available != 0 || cstring(unsafe.Pointer(result.gpus.name)) != "GPU" {
			t.Fatal("GPU presence lost")
		}
		if result.rows.is_service != 1 || cstring(unsafe.Pointer(result.rows.id)) != "app/api" || cstring(unsafe.Pointer(result.disk_mount)) != "/data" {
			t.Fatal("nested fields lost")
		}
		WDResourcesFree(result)
	}
	WDResourcesFree(resourceResult(nil, errors.New("fixture")))
	WDResourcesFree(nil)
	WDPortsFree(nil)
}

func TestOrganizationResultOwnsNestedValues(t *testing.T) {
	snapshot := cloudFixture()
	for i := 0; i < 100; i++ {
		result := organizationResult(snapshot, nil)
		if result.member_count != 1 || result.role_count != 1 || result.access_count != 1 || result.invite_count != 1 {
			t.Fatal("missing organization data")
		}
		if got := cstring(unsafe.Pointer(unsafe.Slice(result.members, 1)[0].email)); got != "person@example.com" {
			t.Fatal(got)
		}
		if got := cstring(unsafe.Pointer(unsafe.Slice(unsafe.Slice(result.roles, 1)[0].permissions, 1)[0])); got != "device:read" {
			t.Fatal(got)
		}
		WDOrganizationFree(result)
	}
	WDOrganizationFree(organizationResult(nil, errors.New("denied")))
	WDOrganizationFree(nil)
}

func cloudFixture() *commands.DesktopOrganization {
	return &commands.DesktopOrganization{
		Organization: &cloudpb.Organization{Name: "Example"},
		Members:      []commands.DesktopCloudMember{{Member: &cloudpb.OrganizationMember{UserId: "user", RoleId: "role"}, Email: "person@example.com"}},
		Roles:        []*cloudpb.Role{{Id: "role", Permissions: []string{"device:read"}}},
		Devices:      []*cloudpb.Asset{{Id: "device", Name: "Camera"}},
		Access:       []*cloudpb.AccessEntry{{Id: "entry", PrincipalId: "user", Effect: cloudpb.AccessEffect_ACCESS_EFFECT_DENY}},
		Invites:      []*invite.OrganizationInvite{{Id: "invite", Role: "viewer"}},
		Permissions:  []string{"member:add"},
	}
}
