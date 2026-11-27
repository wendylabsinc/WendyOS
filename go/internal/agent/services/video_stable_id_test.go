package services

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// The incident these tests exist for: a reboot re-ordered USB enumeration on a
// two-camera device, so every config that had pinned /dev/videoN was addressing
// the other camera. It went unnoticed for hours because opening the wrong
// camera SUCCEEDS -- the tile showed a real picture, from the wrong sensor, and
// nothing anywhere reported an error.
//
//	before   Arducam video0,1,2,3   TC001 video4,5
//	after    Arducam video2,3,4,5   TC001 video0,1
//
// The names are the real ones from that device.
const (
	arducamByID = "usb-Arducam_Technology_Co.__Ltd._USB_2.0_Camera_SN0001-video-index0"
	topdonByID  = "usb-Camera_USB_Camera_EA6913883-video-index0"
)

// fakeV4L builds a /dev/v4l-shaped tree: real files standing in for device
// nodes, and a by-id directory of symlinks pointing at them.
func fakeV4L(t *testing.T, links map[string]string) (byIDDir string, devDir string) {
	t.Helper()
	root := t.TempDir()
	devDir = filepath.Join(root, "dev")
	byIDDir = filepath.Join(root, "by-id")
	for _, d := range []string{devDir, byIDDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, node := range links {
		nodePath := filepath.Join(devDir, node)
		if err := os.WriteFile(nodePath, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(nodePath, filepath.Join(byIDDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	return byIDDir, devDir
}

// resolved matches what readStableNames keys on: EvalSymlinks returns the real
// path, and on macOS /var and /tmp are themselves symlinks.
func resolved(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// noSysfs stands in for a host whose sysfs cannot be read. The tests that pass
// it are about udev's names alone.
const noSysfs = "/nonexistent/sys/class/video4linux"

// fakeCamera is one physical USB camera: the port it is plugged into and what
// the kernel read off its descriptors. vendor and product are the numeric IDs
// every USB device has. The strings are optional, as they are on real
// hardware: an empty one is a camera with no such string, which has no file
// for it in sysfs.
type fakeCamera struct {
	port                        string
	vendor, product             string
	manufacturer, model, serial string
}

// fakeNode is one /dev/videoN, the udev links that point at it, and its place
// in sysfs. A nil camera is a node with no USB device behind it, as on a CSI
// sensor.
type fakeNode struct {
	name         string
	camera       *fakeCamera
	iface, index string
	byID, byPath string
}

// fakeHost is the three places the agent reads a camera's identity from: the
// device nodes, udev's /dev/v4l links, and the kernel's sysfs tree.
type fakeHost struct {
	byIDDir, byPathDir, sysDir string
}

func (h fakeHost) names() map[string]stableNames {
	return readStableNames(h.byIDDir, h.byPathDir, h.sysDir)
}

// byNode re-keys the names by node ("video0"), so a test need not know where
// the temp dir resolved to.
func byNode(names map[string]stableNames) map[string]stableNames {
	out := map[string]stableNames{}
	for dev, n := range names {
		out[filepath.Base(dev)] = n
	}
	return out
}

func newFakeHost(t *testing.T, nodes ...fakeNode) fakeHost {
	t.Helper()
	root := t.TempDir()
	devDir := filepath.Join(root, "dev")
	h := fakeHost{
		byIDDir:   filepath.Join(root, "by-id"),
		byPathDir: filepath.Join(root, "by-path"),
		sysDir:    filepath.Join(root, "sys", "class", "video4linux"),
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, content string) {
		t.Helper()
		must(os.MkdirAll(filepath.Dir(path), 0o755))
		must(os.WriteFile(path, []byte(content), 0o644))
	}
	for _, d := range []string{devDir, h.byIDDir, h.byPathDir, h.sysDir} {
		must(os.MkdirAll(d, 0o755))
	}
	for _, n := range nodes {
		dev := filepath.Join(devDir, n.name)
		write(dev, "")
		if n.byID != "" {
			must(os.Symlink(dev, filepath.Join(h.byIDDir, n.byID)))
		}
		if n.byPath != "" {
			must(os.Symlink(dev, filepath.Join(h.byPathDir, n.byPath)))
		}
		// The kernel's own layout: a node hangs off the interface it was
		// registered on, `device` points back up at that interface, and the
		// USB device one level above carries the descriptors.
		parent := filepath.Join(root, "sys", "devices", "platform", "fe801000.csi")
		if c := n.camera; c != nil {
			usb := filepath.Join(root, "sys", "devices", "usb1", c.port)
			write(filepath.Join(usb, "idVendor"), c.vendor+"\n")
			write(filepath.Join(usb, "idProduct"), c.product+"\n")
			for attr, value := range map[string]string{
				"manufacturer": c.manufacturer, "product": c.model, "serial": c.serial,
			} {
				if value != "" {
					write(filepath.Join(usb, attr), value+"\n")
				}
			}
			iface := n.iface
			if iface == "" {
				iface = "1.0"
			}
			parent = filepath.Join(usb, c.port+":"+iface)
		}
		nodeDir := filepath.Join(parent, "video4linux", n.name)
		write(filepath.Join(nodeDir, "index"), n.index+"\n")
		must(os.Symlink(filepath.Join("..", "..", "..", filepath.Base(parent)), filepath.Join(nodeDir, "device")))
		must(os.Symlink(nodeDir, filepath.Join(h.sysDir, n.name)))
	}
	return h
}

func TestByIDSurvivesTheEnumerationSwap(t *testing.T) {
	// The same two cameras, before and after the reboot that swapped them.
	cases := []struct {
		label           string
		links           map[string]string
		arducam, topdon string
	}{
		{
			label:   "before reboot",
			links:   map[string]string{arducamByID: "video0", topdonByID: "video4"},
			arducam: "video0", topdon: "video4",
		},
		{
			label:   "after reboot",
			links:   map[string]string{arducamByID: "video2", topdonByID: "video0"},
			arducam: "video2", topdon: "video0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			byIDDir, devDir := fakeV4L(t, tc.links)
			names := readStableNames(byIDDir, filepath.Join(byIDDir, "absent"), noSysfs)

			for _, want := range []struct {
				byID, node string
			}{{arducamByID, tc.arducam}, {topdonByID, tc.topdon}} {
				got, ok := names[resolved(t, filepath.Join(devDir, want.node))]
				if !ok {
					t.Fatalf("%s: no entry for %s", tc.label, want.node)
				}
				if got.byID != want.byID {
					t.Errorf("%s: %s -> by-id %q, want %q",
						tc.label, want.node, got.byID, want.byID)
				}
			}
		})
	}
}

func TestByPathIsTheFallbackNotASecondIdentity(t *testing.T) {
	// A camera with both names reports one identity, the by-id one: it is the
	// name that survives a move to another port. The port name only becomes
	// the identity when the by-id name cannot be one -- here a CSI sensor,
	// which udev gives a by-path link and never a by-id. (The other case, two
	// cameras that share one by-id name, has its own tests below.)
	const portName = "platform-xhci-hcd.0-usb-0:2.3:1.0-video-index0"
	const csiPort = "platform-fe801000.csi-video-index0"
	names := newFakeHost(t,
		// video2 has both names; video3 has only its port.
		fakeNode{name: "video2", index: "0", byID: arducamByID, byPath: portName,
			camera: &fakeCamera{port: "1-2.3", vendor: "0c45", product: "6366", serial: "SN0001"}},
		fakeNode{name: "video3", index: "0", byPath: csiPort},
	).names()
	if got, want := byNode(names)["video2"].stableID(), "by-id:"+arducamByID; got != want {
		t.Errorf("stableID = %q, want %q", got, want)
	}
	if got, want := byNode(names)["video3"].stableID(), "by-path:"+csiPort; got != want {
		t.Errorf("camera with no by-id: stableID = %q, want %q", got, want)
	}
	// Both names still resolve for the camera that has both: pinning to a port
	// is allowed when it is asked for explicitly.
	if got, ok := resolveStableID(names, "by-path:"+portName); !ok || got != 2 {
		t.Errorf("explicit by-path resolved to (%d, %v), want (2, true)", got, ok)
	}
}

func TestMissingDirectoriesAreNotAnError(t *testing.T) {
	// A device with no /dev/v4l at all must still enumerate. Losing the
	// identity is survivable; failing the listing is not.
	if got := readStableNames("/nonexistent/by-id", "/nonexistent/by-path", noSysfs); len(got) != 0 {
		t.Errorf("want empty map, got %v", got)
	}
}

func TestDanglingSymlinkDoesNotCostTheOthers(t *testing.T) {
	byIDDir, devDir := fakeV4L(t, map[string]string{topdonByID: "video0"})
	if err := os.Symlink(filepath.Join(devDir, "video9"), // never created
		filepath.Join(byIDDir, "usb-Broken-video-index0")); err != nil {
		t.Fatal(err)
	}
	names := readStableNames(byIDDir, "", noSysfs)
	if got := names[resolved(t, filepath.Join(devDir, "video0"))].byID; got != topdonByID {
		t.Errorf("a dangling symlink cost a healthy camera its identity: got %q", got)
	}
}

func TestResolveStableIDIsExactNotPrefix(t *testing.T) {
	// A prefix rule would make "…_SN1-video-index0" match "…_SN10-video-index0"
	// and hand back one of them silently -- the same class of wrong-camera bug
	// this whole change exists to remove.
	names := map[string]stableNames{
		"/dev/video0": {byID: "usb-Acme_Cam_SN1-video-index0"},
		"/dev/video2": {byID: "usb-Acme_Cam_SN10-video-index0"},
	}
	got, ok := resolveStableID(names, "by-id:usb-Acme_Cam_SN1-video-index0")
	if !ok || got != 0 {
		t.Fatalf("exact name resolved to (%d, %v), want (0, true)", got, ok)
	}
	if _, ok := resolveStableID(names, "by-id:usb-Acme_Cam_SN"); ok {
		t.Error("a prefix matched; resolution must be exact")
	}
	if _, ok := resolveStableID(names, ""); ok {
		t.Error("an empty name matched")
	}
}

func TestResolveStableIDRequiresItsScheme(t *testing.T) {
	// The tag is not decoration. An untagged name would have to be guessed
	// against both namespaces, which is how a name meant for one camera
	// quietly resolves to another.
	names := map[string]stableNames{
		"/dev/video0": {byID: topdonByID, byPath: "platform-xhci-hcd.0-usb-0:2.1:1.0-video-index0"},
	}
	for _, want := range []string{topdonByID, "by-serial:" + topdonByID, "by-id:", "by-path:"} {
		if _, ok := resolveStableID(names, want); ok {
			t.Errorf("%q resolved; only a tagged, non-empty name may", want)
		}
	}
	if _, ok := resolveStableID(names, "by-id:"+topdonByID); !ok {
		t.Error("the tagged name did not resolve")
	}
}

func TestResolveStableIDReportsNoMatch(t *testing.T) {
	names := map[string]stableNames{"/dev/video0": {byID: topdonByID}}
	if _, ok := resolveStableID(names, "by-id:"+arducamByID); ok {
		t.Error("an absent camera resolved; the caller must be able to refuse")
	}
	// A by-path name for a camera that only has a by-id must not fall through
	// to it: they are different guarantees.
	if _, ok := resolveStableID(names, "by-path:platform-xhci-hcd.0-usb-0:2.1:1.0-video-index0"); ok {
		t.Error("a by-path name matched a camera that has no by-path")
	}
}

// ── two cameras, one by-id name ─────────────────────────────────────────────

// Two cameras of one model that share a factory serial, on ports 2.3 and 2.4.
// Each has a capture node (index0) and a UVC metadata node (index1). udev
// builds the by-id name from vendor + model + serial, so the pair has ONE name
// per index between them, and it can only point at one twin.
const (
	arducamMetaByID = "usb-Arducam_Technology_Co.__Ltd._USB_2.0_Camera_SN0001-video-index1"
	twinPortA       = "platform-xhci-hcd.0-usb-0:2.3:1.0-video-index0"
	twinPortAMeta   = "platform-xhci-hcd.0-usb-0:2.3:1.0-video-index1"
	twinPortB       = "platform-xhci-hcd.0-usb-0:2.4:1.0-video-index0"
	twinPortBMeta   = "platform-xhci-hcd.0-usb-0:2.4:1.0-video-index1"
)

// twinHost plugs the pair in. captureLink and metaLink name the nodes udev's
// two colliding by-id links ended up on, which is decided by the order it
// processed the cameras in and so differs from one boot to the next.
func twinHost(t *testing.T, serial, captureLink, metaLink string) fakeHost {
	t.Helper()
	a := &fakeCamera{port: "1-2.3", vendor: "0c45", product: "6366", serial: serial,
		manufacturer: "Arducam Technology Co., Ltd.", model: "USB 2.0 Camera"}
	b := &fakeCamera{port: "1-2.4", vendor: "0c45", product: "6366", serial: serial,
		manufacturer: "Arducam Technology Co., Ltd.", model: "USB 2.0 Camera"}
	nodes := []fakeNode{
		{name: "video0", camera: a, index: "0", byPath: twinPortA},
		{name: "video1", camera: a, index: "1", byPath: twinPortAMeta},
		{name: "video2", camera: b, index: "0", byPath: twinPortB},
		{name: "video3", camera: b, index: "1", byPath: twinPortBMeta},
	}
	for i := range nodes {
		switch nodes[i].name {
		case captureLink:
			nodes[i].byID = arducamByID
		case metaLink:
			nodes[i].byID = arducamMetaByID
		}
	}
	return newFakeHost(t, nodes...)
}

func TestSameSerialTwinsReportTheirPortNotTheSharedName(t *testing.T) {
	// The defect: the twin holding the link reported "by-id:…SN0001…", a
	// caller pinned it, and after a reboot the link pointed at the other twin.
	// Opening the wrong camera succeeds, so nothing reported an error.
	cases := []struct {
		label, serial, captureLink, metaLink string
	}{
		{"first twin holds the links", "SN0001", "video0", "video1"},
		{"second twin holds them after a reboot", "SN0001", "video2", "video3"},
		{"one link each", "SN0001", "video2", "video1"},
		// A model with no serial string at all collides the same way: udev
		// falls back to vendor + model, and every unit of it gets that name.
		{"no serial at all", "", "video0", "video1"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			names := twinHost(t, tc.serial, tc.captureLink, tc.metaLink).names()
			for node, port := range map[string]string{
				"video0": twinPortA, "video1": twinPortAMeta,
				"video2": twinPortB, "video3": twinPortBMeta,
			} {
				if got, want := byNode(names)[node].stableID(), "by-path:"+port; got != want {
					t.Errorf("%s: stableID = %q, want %q", node, got, want)
				}
			}
			// The shared name must not resolve to EITHER twin: whichever one
			// it reached would be right on this boot and wrong on the next.
			for _, shared := range []string{arducamByID, arducamMetaByID} {
				if got, ok := resolveStableID(names, "by-id:"+shared); ok {
					t.Errorf("by-id:%s resolved to video%d; a name two cameras share must be refused", shared, got)
				}
			}
			// The port still tells them apart, and still resolves.
			for port, want := range map[string]uint32{twinPortA: 0, twinPortB: 2} {
				if got, ok := resolveStableID(names, "by-path:"+port); !ok || got != want {
					t.Errorf("by-path:%s resolved to (%d, %v), want (%d, true)", port, got, ok, want)
				}
			}
		})
	}
}

func TestSharedByIDRefusalNamesThePortsToUseInstead(t *testing.T) {
	names := twinHost(t, "SN0001", "video2", "video3").names()
	err := sharedByIDRefusal(names, "by-id:"+arducamByID)
	if err == nil {
		t.Fatal("a by-id name two connected cameras share was not refused")
	}
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", status.Code(err))
	}
	// The refusal has to be usable on a device the operator cannot log into:
	// say why, and name the identity of each camera the name could have meant.
	msg := status.Convert(err).Message()
	for _, want := range []string{
		"by-id:" + arducamByID, "serial",
		"by-path:" + twinPortA, "by-path:" + twinPortB,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal does not mention %q: %s", want, msg)
		}
	}
	// It names the two capture nodes the request could have meant, not every
	// node the two cameras have.
	for _, other := range []string{twinPortAMeta, twinPortBMeta} {
		if strings.Contains(msg, other) {
			t.Errorf("refusal for the capture node offers a metadata node %q: %s", other, msg)
		}
	}
	// Anything else is not this refusal's to make: a port, an unknown name.
	for _, id := range []string{"by-path:" + twinPortA, "by-id:" + topdonByID, "", "by-id:"} {
		if err := sharedByIDRefusal(names, id); err != nil {
			t.Errorf("%q refused as a shared by-id name: %v", id, err)
		}
	}
}

func TestTwinsWithNoPortNameHaveNoIdentity(t *testing.T) {
	// A host whose udev rules publish by-id links but no by-path ones. The
	// shared name is no more of an identity for having nothing to fall back
	// to: the twin holding it reports none, exactly like a camera udev never
	// named, and the refusal does not offer a list of nothing.
	names := map[string]stableNames{
		"/dev/video0": {byID: arducamByID, twin: "pair"},
	}
	if got := names["/dev/video0"].stableID(); got != "" {
		t.Errorf("stableID = %q, want none", got)
	}
	if got, ok := resolveStableID(names, "by-id:"+arducamByID); ok {
		t.Errorf("the shared name resolved to video%d; it must be refused", got)
	}
	err := sharedByIDRefusal(names, "by-id:"+arducamByID)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition (%v)", status.Code(err), err)
	}
	if msg := status.Convert(err).Message(); strings.Contains(msg, "[]") || !strings.Contains(msg, "no port-based id") {
		t.Errorf("refusal should say there is no port to use instead: %s", msg)
	}
}

func TestDifferentSerialsAreNotTwins(t *testing.T) {
	// Two cameras of one model with their own serials are exactly what by-id
	// is for, and so is a pair where only one has a serial: udev gives each
	// its own name, and each keeps it. (This model has no manufacturer or
	// product string, so its names are built from the numeric IDs.)
	const (
		firstByID  = "usb-1234_abcd_SN1-video-index0"
		secondByID = "usb-1234_abcd_SN2-video-index0"
		bareByID   = "usb-1234_abcd-video-index0"
	)
	names := newFakeHost(t,
		fakeNode{name: "video0", index: "0", byID: firstByID, byPath: twinPortA,
			camera: &fakeCamera{port: "1-2.3", vendor: "1234", product: "abcd", serial: "SN1"}},
		fakeNode{name: "video2", index: "0", byID: secondByID, byPath: twinPortB,
			camera: &fakeCamera{port: "1-2.4", vendor: "1234", product: "abcd", serial: "SN2"}},
		fakeNode{name: "video4", index: "0", byID: bareByID,
			camera: &fakeCamera{port: "1-2.5", vendor: "1234", product: "abcd"}},
	).names()
	for node, byID := range map[string]string{"video0": firstByID, "video2": secondByID, "video4": bareByID} {
		if got, want := byNode(names)[node].stableID(), "by-id:"+byID; got != want {
			t.Errorf("%s: stableID = %q, want %q", node, got, want)
		}
		if err := sharedByIDRefusal(names, "by-id:"+byID); err != nil {
			t.Errorf("%s refused: %v", byID, err)
		}
	}
	if got, ok := resolveStableID(names, "by-id:"+secondByID); !ok || got != 2 {
		t.Errorf("resolved to (%d, %v), want (2, true)", got, ok)
	}
}

func TestTwinsAreDecidedByWhatTheNameIsMadeOf(t *testing.T) {
	// udev builds the name from the manufacturer and product STRINGS, and only
	// falls back to the numeric IDs for a camera that has none. The numbers
	// alone get both of these wrong.
	t.Run("two models with one name", func(t *testing.T) {
		// Different products of one white-label vendor: different IDs, but both
		// describe themselves as "HD USB Camera" and neither has a serial, so
		// udev gives them one by-id name between them.
		const sharedByID = "usb-HD_USB_Camera_HD_USB_Camera-video-index0"
		names := newFakeHost(t,
			fakeNode{name: "video0", index: "0", byPath: twinPortA,
				camera: &fakeCamera{port: "1-2.3", vendor: "32e4", product: "9230",
					manufacturer: "HD USB Camera", model: "HD USB Camera"}},
			fakeNode{name: "video2", index: "0", byID: sharedByID, byPath: twinPortB,
				camera: &fakeCamera{port: "1-2.4", vendor: "32e4", product: "0234",
					manufacturer: "HD USB Camera", model: "HD USB Camera"}},
		).names()
		for node, port := range map[string]string{"video0": twinPortA, "video2": twinPortB} {
			if got, want := byNode(names)[node].stableID(), "by-path:"+port; got != want {
				t.Errorf("%s: stableID = %q, want %q", node, got, want)
			}
		}
		if got, ok := resolveStableID(names, "by-id:"+sharedByID); ok {
			t.Errorf("the shared name resolved to video%d; it must be refused", got)
		}
	})

	t.Run("no strings at all", func(t *testing.T) {
		// Then the numeric IDs ARE the name, and two units of the model share
		// it.
		const sharedByID = "usb-1234_abcd-video-index0"
		names := newFakeHost(t,
			fakeNode{name: "video0", index: "0", byID: sharedByID, byPath: twinPortA,
				camera: &fakeCamera{port: "1-2.3", vendor: "1234", product: "abcd"}},
			fakeNode{name: "video2", index: "0", byPath: twinPortB,
				camera: &fakeCamera{port: "1-2.4", vendor: "1234", product: "abcd"}},
		).names()
		for node, port := range map[string]string{"video0": twinPortA, "video2": twinPortB} {
			if got, want := byNode(names)[node].stableID(), "by-path:"+port; got != want {
				t.Errorf("%s: stableID = %q, want %q", node, got, want)
			}
		}
	})

	t.Run("one bridge chip under two brands", func(t *testing.T) {
		// The same chip, so the same IDs and the same factory-default serial,
		// but each brand wrote its own strings. udev gives each its own name,
		// and nothing about either is ambiguous.
		const otherByID = "usb-HD_USB_Camera_HD_USB_Camera_SN0001-video-index0"
		names := newFakeHost(t,
			fakeNode{name: "video0", index: "0", byID: arducamByID, byPath: twinPortA,
				camera: &fakeCamera{port: "1-2.3", vendor: "0c45", product: "6366", serial: "SN0001",
					manufacturer: "Arducam Technology Co., Ltd.", model: "USB 2.0 Camera"}},
			fakeNode{name: "video2", index: "0", byID: otherByID, byPath: twinPortB,
				camera: &fakeCamera{port: "1-2.4", vendor: "0c45", product: "6366", serial: "SN0001",
					manufacturer: "HD USB Camera", model: "HD USB Camera"}},
		).names()
		for node, byID := range map[string]string{"video0": arducamByID, "video2": otherByID} {
			if got, want := byNode(names)[node].stableID(), "by-id:"+byID; got != want {
				t.Errorf("%s: stableID = %q, want %q", node, got, want)
			}
		}
		if got, ok := resolveStableID(names, "by-id:"+otherByID); !ok || got != 2 {
			t.Errorf("resolved to (%d, %v), want (2, true)", got, ok)
		}
	})
}

func TestOneCameraWithSeveralNodesIsNotItsOwnTwin(t *testing.T) {
	// Twins are two PHYSICAL cameras. The nodes of one camera share its vendor,
	// product and serial by construction, and must not be mistaken for a pair.
	cam := &fakeCamera{port: "1-2.3", vendor: "0c45", product: "6366", serial: "SN0001"}

	t.Run("capture and metadata nodes", func(t *testing.T) {
		names := newFakeHost(t,
			fakeNode{name: "video0", camera: cam, index: "0", byID: arducamByID, byPath: twinPortA},
			fakeNode{name: "video1", camera: cam, index: "1", byID: arducamMetaByID, byPath: twinPortAMeta},
		).names()
		if got, want := byNode(names)["video0"].stableID(), "by-id:"+arducamByID; got != want {
			t.Errorf("video0: stableID = %q, want %q", got, want)
		}
		if got, want := byNode(names)["video1"].stableID(), "by-id:"+arducamMetaByID; got != want {
			t.Errorf("video1: stableID = %q, want %q", got, want)
		}
		if got, ok := resolveStableID(names, "by-id:"+arducamByID); !ok || got != 0 {
			t.Errorf("resolved to (%d, %v), want (0, true)", got, ok)
		}
	})

	t.Run("two interfaces of one camera", func(t *testing.T) {
		// A camera with two video functions has an index0 on each interface,
		// so they do collide on one by-id link. That is one device describing
		// itself twice, not two devices: the node without the link keeps
		// falling back to its port, as it always has.
		const secondIface = "platform-xhci-hcd.0-usb-0:2.3:1.2-video-index0"
		names := newFakeHost(t,
			fakeNode{name: "video0", camera: cam, iface: "1.0", index: "0", byID: arducamByID, byPath: twinPortA},
			fakeNode{name: "video2", camera: cam, iface: "1.2", index: "0", byPath: secondIface},
		).names()
		if got, want := byNode(names)["video0"].stableID(), "by-id:"+arducamByID; got != want {
			t.Errorf("video0: stableID = %q, want %q", got, want)
		}
		if got, want := byNode(names)["video2"].stableID(), "by-path:"+secondIface; got != want {
			t.Errorf("video2: stableID = %q, want %q", got, want)
		}
		if got, ok := resolveStableID(names, "by-id:"+arducamByID); !ok || got != 0 {
			t.Errorf("resolved to (%d, %v), want (0, true)", got, ok)
		}
	})
}

func TestUnreadableSysfsKeepsTheUdevNames(t *testing.T) {
	// Twins are detected from sysfs. Where it cannot be read there is nothing
	// to detect them with, and the listing must not get worse for it: the
	// names udev published are reported as they were before.
	host := twinHost(t, "SN0001", "video0", "video1")
	names := readStableNames(host.byIDDir, host.byPathDir, noSysfs)
	if got, want := byNode(names)["video0"].stableID(), "by-id:"+arducamByID; got != want {
		t.Errorf("video0: stableID = %q, want %q", got, want)
	}
	if got, want := byNode(names)["video2"].stableID(), "by-path:"+twinPortB; got != want {
		t.Errorf("video2: stableID = %q, want %q", got, want)
	}
	if got, ok := resolveStableID(names, "by-id:"+arducamByID); !ok || got != 0 {
		t.Errorf("resolved to (%d, %v), want (0, true)", got, ok)
	}
}

// ── service level ───────────────────────────────────────────────────────────

func TestListCamerasReportsStableIdentity(t *testing.T) {
	svc := newTestVideoService(
		func() ([]string, error) { return []string{"/dev/video0", "/dev/video2"}, nil },
		func(base string) (string, error) { return base, nil },
	)
	svc.readStableNames = func() map[string]stableNames {
		return map[string]stableNames{
			"/dev/video0": {byID: topdonByID, byPath: "platform-xhci-hcd.0-usb-0:2.1:1.0-video-index0"},
			// No by-id: the identity falls back to the port.
			"/dev/video2": {byPath: "platform-xhci-hcd.0-usb-0:2.3:1.0-video-index0"},
		}
	}
	devices, err := svc.listCameras(context.Background())
	if err != nil {
		t.Fatalf("listCameras: %v", err)
	}
	got := map[uint32]string{}
	for _, d := range devices {
		got[d.GetId()] = d.GetStableId()
	}
	if want := "by-id:" + topdonByID; got[0] != want {
		t.Errorf("video0 stable id = %q, want %q", got[0], want)
	}
	if want := "by-path:platform-xhci-hcd.0-usb-0:2.3:1.0-video-index0"; got[2] != want {
		t.Errorf("video2 stable id = %q, want %q", got[2], want)
	}
}

func TestListCamerasWithoutStableNamesIsUnchanged(t *testing.T) {
	// Every device with no /dev/v4l -- and every existing caller -- must see
	// exactly what it saw before.
	svc := newTestVideoService(
		func() ([]string, error) { return []string{"/dev/video0"}, nil },
		func(base string) (string, error) { return "USB Camera", nil },
	)
	devices, err := svc.listCameras(context.Background())
	if err != nil {
		t.Fatalf("listCameras: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("got %d devices, want 1", len(devices))
	}
	if devices[0].GetStableId() != "" {
		t.Errorf("identity invented from nothing: %q", devices[0].GetStableId())
	}
	if devices[0].GetId() != 0 || devices[0].GetName() != "USB Camera" {
		t.Errorf("existing fields changed: %+v", devices[0])
	}
}

func TestStreamDeviceIDPrefersTheName(t *testing.T) {
	svc := newTestVideoService(nil, nil)
	// The post-reboot layout: the Arducam is on video2 now, and a config still
	// says deviceId 0. The name must win.
	svc.readStableNames = func() map[string]stableNames {
		return map[string]stableNames{
			"/dev/video0": {byID: topdonByID},
			"/dev/video2": {byID: arducamByID},
		}
	}
	got, err := svc.streamDeviceID(&agentpb.StreamVideoRequest{
		DeviceId: 0,
		StableId: "by-id:" + arducamByID,
	})
	if err != nil {
		t.Fatalf("streamDeviceID: %v", err)
	}
	if got != 2 {
		t.Errorf("resolved to %d, want 2 -- the stale deviceId won", got)
	}
}

func TestStreamDeviceIDRefusesAnUnknownName(t *testing.T) {
	// The load-bearing case. Falling back to device_id here would stream
	// whatever the kernel put at that number, succeed, and tell nobody.
	svc := newTestVideoService(nil, nil)
	svc.readStableNames = func() map[string]stableNames {
		return map[string]stableNames{"/dev/video0": {byID: topdonByID}}
	}
	_, err := svc.streamDeviceID(&agentpb.StreamVideoRequest{
		DeviceId: 0,
		StableId: "by-id:" + arducamByID,
	})
	if err == nil {
		t.Fatal("an absent camera resolved; it must refuse rather than fall back")
	}
	if status.Code(err) != codes.NotFound {
		t.Errorf("code = %v, want NotFound", status.Code(err))
	}
	// The error must name what IS there, or the operator is left guessing on a
	// device they may not be able to log into.
	if !strings.Contains(err.Error(), topdonByID) {
		t.Errorf("error does not list the available cameras: %v", err)
	}
}

func TestListCamerasGivesTwinsTheirPorts(t *testing.T) {
	svc := newTestVideoService(
		func() ([]string, error) { return []string{"/dev/video0", "/dev/video2", "/dev/video4"}, nil },
		func(base string) (string, error) { return base, nil },
	)
	svc.readStableNames = func() map[string]stableNames {
		return map[string]stableNames{
			// The twin holding udev's link, and the one that lost it.
			"/dev/video0": {byID: arducamByID, byPath: twinPortA, twin: "pair"},
			"/dev/video2": {byPath: twinPortB, twin: "pair"},
			// A camera with no twin connected is reported exactly as before.
			"/dev/video4": {byID: topdonByID, byPath: "platform-xhci-hcd.0-usb-0:2.1:1.0-video-index0"},
		}
	}
	devices, err := svc.listCameras(context.Background())
	if err != nil {
		t.Fatalf("listCameras: %v", err)
	}
	got := map[uint32]string{}
	for _, d := range devices {
		got[d.GetId()] = d.GetStableId()
	}
	for id, want := range map[uint32]string{
		0: "by-path:" + twinPortA,
		2: "by-path:" + twinPortB,
		4: "by-id:" + topdonByID,
	} {
		if got[id] != want {
			t.Errorf("video%d stable id = %q, want %q", id, got[id], want)
		}
	}
}

func TestStreamDeviceIDRefusesANameTwoCamerasShare(t *testing.T) {
	// A viewer that pinned the by-id name before the second camera was plugged
	// in, or before this agent stopped handing the name out. Resolving it to
	// the twin that holds the link today is the silent wrong-camera failure;
	// "no such camera" would send the operator looking for a missing one.
	svc := newTestVideoService(nil, nil)
	svc.readStableNames = func() map[string]stableNames {
		return map[string]stableNames{
			"/dev/video0": {byID: arducamByID, byPath: twinPortA, twin: "pair"},
			"/dev/video2": {byPath: twinPortB, twin: "pair"},
		}
	}
	_, err := svc.streamDeviceID(&agentpb.StreamVideoRequest{StableId: "by-id:" + arducamByID})
	if err == nil {
		t.Fatal("a by-id name two connected cameras share resolved; it must refuse")
	}
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", status.Code(err))
	}
	for _, want := range []string{"by-path:" + twinPortA, "by-path:" + twinPortB} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q: %v", want, err)
		}
	}
	// Each twin is still addressable by the identity it is listed under.
	got, err := svc.streamDeviceID(&agentpb.StreamVideoRequest{StableId: "by-path:" + twinPortB})
	if err != nil || got != 2 {
		t.Errorf("by-path resolved to (%d, %v), want (2, nil)", got, err)
	}
}

func TestStreamDeviceIDFallsBackToTheNumberWhenUnnamed(t *testing.T) {
	svc := newTestVideoService(nil, nil)
	got, err := svc.streamDeviceID(&agentpb.StreamVideoRequest{DeviceId: 4})
	if err != nil {
		t.Fatalf("streamDeviceID: %v", err)
	}
	if got != 4 {
		t.Errorf("got %d, want 4 -- an unnamed request must behave as before", got)
	}
}
