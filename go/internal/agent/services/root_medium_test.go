package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/gpudiscovery"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// fakeSysfs builds a sysfs tree with relative symlinks, the way the kernel
// lays it out.
type fakeSysfs struct {
	t    *testing.T
	root string
}

func (s fakeSysfs) mkdir(p string) {
	s.t.Helper()
	if err := os.MkdirAll(filepath.Join(s.root, p), 0o755); err != nil {
		s.t.Fatal(err)
	}
}

func (s fakeSysfs) file(p, v string) {
	s.t.Helper()
	s.mkdir(filepath.Dir(p))
	if err := os.WriteFile(filepath.Join(s.root, p), []byte(v+"\n"), 0o444); err != nil {
		s.t.Fatal(err)
	}
}

func (s fakeSysfs) link(p, target string) {
	s.t.Helper()
	s.mkdir(filepath.Dir(p))
	rel, err := filepath.Rel(filepath.Dir(filepath.Join(s.root, p)), filepath.Join(s.root, target))
	if err != nil {
		s.t.Fatal(err)
	}
	if err := os.Symlink(rel, filepath.Join(s.root, p)); err != nil {
		s.t.Fatal(err)
	}
}

// dev makes p a device whose subsystem link points at subsys.
func (s fakeSysfs) dev(p, subsys string) {
	s.mkdir(p)
	s.mkdir(subsys)
	s.link(filepath.Join(p, "subsystem"), subsys)
}

// disk adds parent/block/name with its dev/block entry and returns its path.
func (s fakeSysfs) disk(parent, name, devt string) string {
	d := filepath.Join(parent, "block", name)
	s.dev(d, "class/block")
	s.mkdir(filepath.Join(d, "slaves"))
	s.file(filepath.Join(d, "dev"), devt)
	s.link(filepath.Join(d, "device"), parent)
	s.link(filepath.Join("dev/block", devt), d)
	return d
}

func (s fakeSysfs) part(disk, name, num, devt string) {
	p := filepath.Join(disk, name)
	s.dev(p, "class/block")
	s.file(filepath.Join(p, "partition"), num)
	s.file(filepath.Join(p, "dev"), devt)
	s.link(filepath.Join("dev/block", devt), p)
}

// mmcCard adds an mmc card of the given type under hostDev; "" leaves the
// type attribute out.
func (s fakeSysfs) mmcCard(hostDev, mmcN, typ string) string {
	s.dev(hostDev, "bus/platform")
	cls := filepath.Join(hostDev, "mmc_host", mmcN)
	s.dev(cls, "class/mmc_host")
	card := filepath.Join(cls, mmcN+":0001")
	s.dev(card, "bus/mmc")
	if typ != "" {
		s.file(filepath.Join(card, "type"), typ)
	}
	return card
}

// scsiDevice adds hostN/target0:0:0/0:0:0:0 under parent with the host's
// proc_name and returns the SCSI device path.
func (s fakeSysfs) scsiDevice(parent, procName string) string {
	host := filepath.Join(parent, "host0")
	s.dev(host, "bus/scsi")
	s.dev(filepath.Join(host, "scsi_host", "host0"), "class/scsi_host")
	s.file(filepath.Join(host, "scsi_host", "host0", "proc_name"), procName)
	tgt := filepath.Join(host, "target0:0:0")
	s.dev(tgt, "bus/scsi")
	sd := filepath.Join(tgt, "0:0:0:0")
	s.dev(sd, "bus/scsi")
	return sd
}

// usbInterface adds a USB device chain under controller and returns the
// interface path that the SCSI host hangs off.
func (s fakeSysfs) usbInterface(controller string, chain ...string) string {
	s.dev(controller, "bus/platform")
	p := controller
	for _, c := range chain {
		p = filepath.Join(p, c)
		s.dev(p, "bus/usb")
	}
	return p
}

func TestRootMediumFromSysfs(t *testing.T) {
	cases := []struct {
		name       string
		build      func(s fakeSysfs)
		id         string
		wantMedium string
		wantDisk   string
		wantErr    bool
	}{
		{
			name: "pi 5 sd card",
			build: func(s fakeSysfs) {
				card := s.mmcCard("devices/platform/axi/1000fff000.mmc", "mmc0", "SD")
				s.part(s.disk(card, "mmcblk0", "179:0"), "mmcblk0p2", "2", "179:2")
			},
			id: "179:2", wantMedium: "sd", wantDisk: "mmcblk0",
		},
		{
			name: "pi 4 sd card on a controller named emmc2",
			build: func(s fakeSysfs) {
				card := s.mmcCard("devices/platform/emmc2bus/fe340000.mmc", "mmc1", "SD")
				s.part(s.disk(card, "mmcblk1", "179:0"), "mmcblk1p3", "3", "179:3")
			},
			id: "179:3", wantMedium: "sd", wantDisk: "mmcblk1",
		},
		{
			name: "orin nano sd card",
			build: func(s fakeSysfs) {
				card := s.mmcCard("devices/platform/bus@0/3400000.mmc", "mmc1", "SD")
				s.part(s.disk(card, "mmcblk1", "179:32"), "mmcblk1p1", "1", "179:33")
			},
			id: "179:33", wantMedium: "sd", wantDisk: "mmcblk1",
		},
		{
			name: "agx orin emmc",
			build: func(s fakeSysfs) {
				card := s.mmcCard("devices/platform/bus@0/3460000.mmc", "mmc0", "MMC")
				s.part(s.disk(card, "mmcblk0", "179:0"), "mmcblk0p1", "1", "179:1")
			},
			id: "179:1", wantMedium: "emmc", wantDisk: "mmcblk0",
		},
		{
			name: "sd combo card",
			build: func(s fakeSysfs) {
				card := s.mmcCard("devices/platform/axi/1000fff000.mmc", "mmc0", "SDcombo")
				s.part(s.disk(card, "mmcblk0", "179:0"), "mmcblk0p2", "2", "179:2")
			},
			id: "179:2", wantMedium: "sd", wantDisk: "mmcblk0",
		},
		{
			name: "mmc card of another type is left to the image",
			build: func(s fakeSysfs) {
				card := s.mmcCard("devices/platform/axi/1000fff000.mmc", "mmc0", "SDIO")
				s.part(s.disk(card, "mmcblk0", "179:0"), "mmcblk0p2", "2", "179:2")
			},
			id: "179:2", wantMedium: "", wantDisk: "mmcblk0",
		},
		{
			name: "mmc card without a readable type is left to the image",
			build: func(s fakeSysfs) {
				card := s.mmcCard("devices/platform/axi/1000fff000.mmc", "mmc0", "")
				s.part(s.disk(card, "mmcblk0", "179:0"), "mmcblk0p2", "2", "179:2")
			},
			id: "179:2", wantMedium: "", wantDisk: "mmcblk0",
		},
		{
			name: "uno q emmc root past the first minors, beside a boot partition disk",
			build: func(s fakeSysfs) {
				card := s.mmcCard("devices/platform/soc@0/4744000.mmc", "mmc0", "MMC")
				s.part(s.disk(card, "mmcblk0", "179:0"), "mmcblk0p69", "69", "259:37")
				s.disk(card, "mmcblk0boot0", "179:32")
			},
			id: "259:37", wantMedium: "emmc", wantDisk: "mmcblk0",
		},
		{
			name: "pi 5 nvme",
			build: func(s fakeSysfs) {
				ctrl := "devices/platform/axi/1000110000.pcie/pci0001:00/0001:00:00.0/0001:01:00.0/nvme/nvme0"
				s.dev(ctrl, "class/nvme")
				s.part(s.disk(ctrl, "nvme0n1", "259:0"), "nvme0n1p2", "2", "259:2")
			},
			id: "259:2", wantMedium: "nvme", wantDisk: "nvme0n1",
		},
		{
			name: "nvme multipath head under the virtual subsystem",
			build: func(s fakeSysfs) {
				head := "devices/virtual/nvme-subsystem/nvme-subsys0"
				s.dev(head, "class/nvme-subsystem")
				s.part(s.disk(head, "nvme0n1", "259:0"), "nvme0n1p2", "2", "259:2")
			},
			id: "259:2", wantMedium: "nvme", wantDisk: "nvme0n1",
		},
		{
			name: "pi 3 usb stick on usb-storage",
			build: func(s fakeSysfs) {
				intf := s.usbInterface("devices/platform/soc/3f980000.usb", "usb1", "1-1", "1-1.1", "1-1.1.2", "1-1.1.2:1.0")
				s.part(s.disk(s.scsiDevice(intf, "usb-storage"), "sda", "8:0"), "sda2", "2", "8:2")
			},
			id: "8:2", wantMedium: "usb", wantDisk: "sda",
		},
		{
			name: "pi 4 usb disk on uas, which sets no proc_name",
			build: func(s fakeSysfs) {
				intf := s.usbInterface("devices/platform/scb/fd500000.pcie/pci0000:00/0000:00:00.0/0000:01:00.0", "usb2", "2-1", "2-1:1.0")
				s.part(s.disk(s.scsiDevice(intf, "(null)"), "sda", "8:0"), "sda3", "3", "8:3")
			},
			id: "8:3", wantMedium: "usb", wantDisk: "sda",
		},
		{
			name: "nvme drive in a usb enclosure",
			build: func(s fakeSysfs) {
				intf := s.usbInterface("devices/platform/axi/1000120000.pcie/1f00200000.usb/xhci-hcd.0", "usb2", "2-1", "2-1:1.0")
				s.part(s.disk(s.scsiDevice(intf, "(null)"), "sda", "8:0"), "sda2", "2", "8:2")
			},
			id: "8:2", wantMedium: "usb", wantDisk: "sda",
		},
		{
			name: "usb stick formatted without a partition table",
			build: func(s fakeSysfs) {
				intf := s.usbInterface("devices/platform/soc/3f980000.usb", "usb1", "1-1", "1-1:1.0")
				s.disk(s.scsiDevice(intf, "usb-storage"), "sda", "8:0")
			},
			id: "8:0", wantMedium: "usb", wantDisk: "sda",
		},
		{
			name: "iq-9075 ufs",
			build: func(s fakeSysfs) {
				ctrl := "devices/platform/soc@0/1d84000.ufshc"
				s.dev(ctrl, "bus/platform")
				s.part(s.disk(s.scsiDevice(ctrl, "ufshcd"), "sda", "8:0"), "sda3", "3", "8:3")
			},
			id: "8:3", wantMedium: "ufs", wantDisk: "sda",
		},
		{
			name: "iq-8275 ufs on a node named ufs",
			build: func(s fakeSysfs) {
				ctrl := "devices/platform/soc@0/1d84000.ufs"
				s.dev(ctrl, "bus/platform")
				s.part(s.disk(s.scsiDevice(ctrl, "ufshcd"), "sda", "8:0"), "sda4", "4", "8:4")
			},
			id: "8:4", wantMedium: "ufs", wantDisk: "sda",
		},
		{
			name: "sata disk is left to the image",
			build: func(s fakeSysfs) {
				port := "devices/pci0000:00/0000:00:17.0/ata1"
				s.dev("devices/pci0000:00/0000:00:17.0", "bus/pci")
				s.mkdir(port)
				s.part(s.disk(s.scsiDevice(port, "ahci"), "sda", "8:0"), "sda3", "3", "8:3")
			},
			id: "8:3", wantMedium: "", wantDisk: "sda",
		},
		{
			name: "virtio-scsi disk is left to the image",
			build: func(s fakeSysfs) {
				v := "devices/pci0000:00/0000:00:05.0/virtio2"
				s.dev(v, "bus/virtio")
				s.part(s.disk(s.scsiDevice(v, "virtio_scsi"), "sda", "8:0"), "sda2", "2", "8:2")
			},
			id: "8:2", wantMedium: "", wantDisk: "sda",
		},
		{
			name: "virtio-blk disk is left to the image",
			build: func(s fakeSysfs) {
				v := "devices/pci0000:00/0000:00:04.0/virtio1"
				s.dev(v, "bus/virtio")
				s.part(s.disk(v, "vda", "253:0"), "vda2", "2", "253:2")
			},
			id: "253:2", wantMedium: "", wantDisk: "vda",
		},
		{
			name: "loop device is left to the image",
			build: func(s fakeSysfs) {
				s.disk("devices/virtual", "loop0", "7:0")
			},
			id: "7:0", wantMedium: "", wantDisk: "loop0",
		},
		{
			name: "device-mapper layer over an sd card",
			build: func(s fakeSysfs) {
				card := s.mmcCard("devices/platform/axi/1000fff000.mmc", "mmc0", "SD")
				disk := s.disk(card, "mmcblk0", "179:0")
				s.part(disk, "mmcblk0p2", "2", "179:2")
				dm := s.disk("devices/virtual", "dm-0", "252:0")
				s.link(filepath.Join(dm, "slaves", "mmcblk0p2"), filepath.Join(disk, "mmcblk0p2"))
			},
			id: "252:0", wantMedium: "sd", wantDisk: "mmcblk0",
		},
		{
			name: "device-mapper target over several partitions",
			build: func(s fakeSysfs) {
				ctrl := "devices/virtual/nvme-subsystem/nvme-subsys0"
				s.dev(ctrl, "class/nvme-subsystem")
				a := s.disk(ctrl, "nvme0n1", "259:0")
				s.part(a, "nvme0n1p1", "1", "259:1")
				s.part(a, "nvme0n1p2", "2", "259:2")
				dm := s.disk("devices/virtual", "dm-0", "252:0")
				s.link(filepath.Join(dm, "slaves", "nvme0n1p1"), filepath.Join(a, "nvme0n1p1"))
				s.link(filepath.Join(dm, "slaves", "nvme0n1p2"), filepath.Join(a, "nvme0n1p2"))
			},
			id: "252:0", wantErr: true,
		},
		{
			name:    "overlay or other anonymous root has no block device",
			build:   func(s fakeSysfs) { s.mkdir("dev/block") },
			id:      "0:25",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := fakeSysfs{t: t, root: t.TempDir()}
			tc.build(s)
			medium, disk, err := rootMediumFromSysfs(s.root, tc.id)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("got medium %q disk %q, want an error", medium, disk)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if medium != tc.wantMedium || disk != tc.wantDisk {
				t.Fatalf("got (%q, %q), want (%q, %q)", medium, disk, tc.wantMedium, tc.wantDisk)
			}
		})
	}
}

func TestPickStorageMedium(t *testing.T) {
	cases := []struct {
		name, booted, image, want string
	}{
		{"booted from sd under a stale nvme image value", "sd", "nvme", "sd"},
		{"image without a storage value booted from nvme", "nvme", "", "nvme"},
		{"undetected medium keeps the image value", "", "ufs", "ufs"},
		{"undetected medium keeps the image fallback", "", "disk", "disk"},
		{"nothing known", "", "", ""},
		{"agreement", "emmc", "emmc", "emmc"},
		{"usb boot of an sd image", "usb", "sd", "usb"},
		{"usb boot of an image without a storage value", "usb", "", "usb"},
		{"nvme boot of an image built for any disk", "nvme", "disk", "nvme"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pickStorageMedium(tc.booted, tc.image); got != tc.want {
				t.Fatalf("pickStorageMedium(%q, %q) = %q, want %q", tc.booted, tc.image, got, tc.want)
			}
		})
	}
}

func TestGetAgentVersionReportsTheBootMedium(t *testing.T) {
	noGPU := func() []gpudiscovery.Device { return nil }
	deviceType := filepath.Join(t.TempDir(), "device-type")
	if err := os.WriteFile(deviceType, []byte("BOARD=raspberry-pi-5\nMACHINE=raspberrypi5-nvme-wendyos\nSTORAGE=nvme\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	probe := func(medium string, err error) func() (string, string, error) {
		return func() (string, string, error) { return medium, "mmcblk0", err }
	}
	cases := []struct {
		name  string
		probe func() (string, string, error)
		want  string
	}{
		{"detected medium replaces a stale image value", probe("sd", nil), "sd"},
		{"unrecognised disk keeps the image value", probe("", nil), "nvme"},
		{"failed probe keeps the image value", probe("sd", errors.New("no sysfs")), "nvme"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &AgentService{deviceTypePath: deviceType, rootMedium: tc.probe, discoverGPUs: noGPU}
			resp, err := s.GetAgentVersion(context.Background(), &agentpb.GetAgentVersionRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if resp.GetDeviceType() != "raspberry-pi-5" || resp.GetStorageMedium() != tc.want {
				t.Fatalf("device type %q, storage medium %q; want raspberry-pi-5, %q", resp.GetDeviceType(), resp.GetStorageMedium(), tc.want)
			}
		})
	}
}

func TestGetAgentVersionSkipsTheProbeOffWendyOS(t *testing.T) {
	probed := false
	s := &AgentService{
		deviceTypePath: filepath.Join(t.TempDir(), "missing"),
		rootMedium:     func() (string, string, error) { probed = true; return "nvme", "nvme0n1", nil },
		discoverGPUs:   func() []gpudiscovery.Device { return nil },
	}
	resp, err := s.GetAgentVersion(context.Background(), &agentpb.GetAgentVersionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if probed || resp.StorageMedium != nil {
		t.Fatalf("probed %v, storage medium %q; want no probe and no medium", probed, resp.GetStorageMedium())
	}
}
