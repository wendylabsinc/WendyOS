// Package hardware implements system hardware discovery by reading sysfs, /dev, and /proc.
package hardware

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/audio"
	"github.com/wendylabsinc/wendy/go/internal/agent/camera"
	"github.com/wendylabsinc/wendy/go/internal/agent/gpudiscovery"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// SystemHardwareDiscoverer discovers hardware by probing the Linux sysfs/devfs/procfs.
type SystemHardwareDiscoverer struct {
	logger             *zap.Logger
	discoverGPUs       func() []gpudiscovery.Device
	classifyTransport  func(base string) (camera.Transport, string)
	enumerateLibcamera func(ctx context.Context) (map[string]string, error)
}

func NewSystemHardwareDiscoverer(logger *zap.Logger) *SystemHardwareDiscoverer {
	return &SystemHardwareDiscoverer{
		logger:             logger,
		classifyTransport:  camera.Classify,
		enumerateLibcamera: camera.EnumerateLibcamera,
	}
}

// Discover probes the system for hardware capabilities, optionally filtered by category.
func (d *SystemHardwareDiscoverer) Discover(ctx context.Context, categoryFilter string) ([]*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability, error) {
	type discoverer struct {
		category string
		fn       func() []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability
	}

	discoverers := []discoverer{
		{"gpu", d.discoverGPU},
		{"usb", d.discoverUSB},
		{"i2c", d.discoverI2C},
		{"spi", d.discoverSPI},
		{"gpio", d.discoverGPIO},
		{"camera", func() []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability { return d.discoverCamera(ctx) }},
		{"audio", func() []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability { return d.discoverAudio(ctx) }},
		{"network", d.discoverNetwork},
		{"storage", d.discoverStorage},
	}

	var caps []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability
	for _, disc := range discoverers {
		if categoryFilter != "" && disc.category != categoryFilter {
			continue
		}
		caps = append(caps, disc.fn()...)
	}

	d.logger.Info("Hardware discovery completed", zap.Int("capabilities", len(caps)))
	return caps, nil
}

// discoverGPU uses the same injectable discovery as device metadata.
func (d *SystemHardwareDiscoverer) discoverGPU() []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability {
	probe := d.discoverGPUs
	if probe == nil {
		probe = gpudiscovery.Host
	}
	var caps []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability
	for _, gpu := range probe() {
		caps = append(caps, &agentpb.ListHardwareCapabilitiesResponse_HardwareCapability{
			Category: "gpu", DevicePath: gpu.Path, Description: strings.TrimSpace(gpu.Vendor + " GPU " + gpu.Driver),
			Properties: map[string]string{"vendor": gpu.Vendor, "driver": gpu.Driver, "compute_backends": strings.Join(gpu.ComputeBackends, ",")},
		})
	}
	return caps
}

// discoverUSB enumerates USB devices from sysfs.
func (d *SystemHardwareDiscoverer) discoverUSB() []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability {
	var caps []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability

	usbPath := "/sys/bus/usb/devices"
	entries, err := os.ReadDir(usbPath)
	if err != nil {
		return nil
	}

	for _, entry := range entries {
		devDir := filepath.Join(usbPath, entry.Name())

		// Read product name.
		product := readSysfsFile(filepath.Join(devDir, "product"))
		if product == "" {
			continue
		}

		vendor := readSysfsFile(filepath.Join(devDir, "idVendor"))
		prodID := readSysfsFile(filepath.Join(devDir, "idProduct"))

		name := product
		if vendor != "" && prodID != "" {
			name = fmt.Sprintf("%s (%s:%s)", product, vendor, prodID)
		}

		caps = append(caps, &agentpb.ListHardwareCapabilitiesResponse_HardwareCapability{
			Category:    "usb",
			DevicePath:  devDir,
			Description: name,
		})
	}

	return caps
}

// discoverI2C enumerates I2C bus devices.
func (d *SystemHardwareDiscoverer) discoverI2C() []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability {
	var caps []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability

	for i := 0; i < 32; i++ {
		path := fmt.Sprintf("/dev/i2c-%d", i)
		if _, err := os.Stat(path); err == nil {
			caps = append(caps, &agentpb.ListHardwareCapabilitiesResponse_HardwareCapability{
				Category:    "i2c",
				DevicePath:  path,
				Description: fmt.Sprintf("I2C bus %d", i),
			})
		}
	}

	return caps
}

// discoverSPI enumerates SPI devices.
func (d *SystemHardwareDiscoverer) discoverSPI() []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability {
	var caps []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability

	entries, err := filepath.Glob("/dev/spidev*")
	if err != nil {
		return nil
	}

	for _, path := range entries {
		caps = append(caps, &agentpb.ListHardwareCapabilitiesResponse_HardwareCapability{
			Category:    "spi",
			DevicePath:  path,
			Description: filepath.Base(path),
		})
	}

	return caps
}

// discoverGPIO enumerates GPIO chip devices.
func (d *SystemHardwareDiscoverer) discoverGPIO() []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability {
	var caps []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability

	gpioPath := "/sys/class/gpio"
	entries, err := os.ReadDir(gpioPath)
	if err != nil {
		return nil
	}

	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "gpiochip") {
			continue
		}

		devPath := filepath.Join(gpioPath, entry.Name())
		label := readSysfsFile(filepath.Join(devPath, "label"))
		ngpio := readSysfsFile(filepath.Join(devPath, "ngpio"))

		name := entry.Name()
		if label != "" {
			name = fmt.Sprintf("%s (%s, %s lines)", entry.Name(), label, ngpio)
		}

		caps = append(caps, &agentpb.ListHardwareCapabilitiesResponse_HardwareCapability{
			Category:    "gpio",
			DevicePath:  devPath,
			Description: name,
		})
	}

	return caps
}

// discoverCamera enumerates V4L2 video devices and classifies each one as
// USB, CSI, or unknown. Transport and kernel driver are surfaced via the
// HardwareCapability.properties map (which is additive, so no proto change is
// required). When exactly one CSI device and one libcamera entry are found,
// the libcamera id is also attached.
func (d *SystemHardwareDiscoverer) discoverCamera(ctx context.Context) []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability {
	var caps []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability

	entries, err := filepath.Glob("/dev/video*")
	if err != nil {
		return nil
	}

	libcameraIDs, libErr := d.enumerateLibcamera(ctx)
	if libErr != nil {
		d.logger.Debug("libcamera enumeration failed", zap.Error(libErr))
	}

	type pending struct {
		cap *agentpb.ListHardwareCapabilitiesResponse_HardwareCapability
		csi bool
	}
	var pendings []pending

	for _, path := range entries {
		base := filepath.Base(path)
		devNum := strings.TrimPrefix(base, "video")

		description := base
		sysName := filepath.Join("/sys/class/video4linux", base, "name")
		if data, err := os.ReadFile(sysName); err == nil {
			description = fmt.Sprintf("%s (%s)", strings.TrimSpace(string(data)), "video"+devNum)
		}

		transport, driver := d.classifyTransport(base)
		props := map[string]string{"transport": transport.String()}
		if driver != "" {
			props["driver"] = driver
		}

		c := &agentpb.ListHardwareCapabilitiesResponse_HardwareCapability{
			Category:    "camera",
			DevicePath:  path,
			Description: description,
			Properties:  props,
		}
		pendings = append(pendings, pending{cap: c, csi: transport == camera.TransportCSI})
		caps = append(caps, c)
	}

	// Only attach a libcamera id in the unambiguous single-CSI / single-libcamera case.
	csiCount := 0
	var solo *pending
	for i := range pendings {
		if pendings[i].csi {
			csiCount++
			solo = &pendings[i]
		}
	}
	if csiCount == 1 && len(libcameraIDs) == 1 {
		for id := range libcameraIDs {
			solo.cap.Properties["libcamera_id"] = id
		}
	}

	return caps
}

// discoverAudio enumerates audio devices from the PipeWire graph, which covers
// sound cards and the Bluetooth endpoints that have none.
func (d *SystemHardwareDiscoverer) discoverAudio(ctx context.Context) []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability {
	nodes, defaults, err := audio.ListNodes(ctx)
	if err != nil {
		d.logger.Debug("Audio discovery skipped", zap.Error(err))
		return nil
	}

	var caps []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability
	for _, n := range nodes {
		direction := "source"
		if n.IsSink {
			direction = "sink"
		}
		props := map[string]string{
			"node_id":   strconv.FormatUint(uint64(n.ID), 10),
			"direction": direction,
		}
		if defaults.IsDefault(n) {
			props["default"] = "true"
		}
		caps = append(caps, &agentpb.ListHardwareCapabilitiesResponse_HardwareCapability{
			Category: "audio",
			// The node name, not a /dev path: Bluetooth endpoints have none.
			DevicePath:  n.Name,
			Description: n.Description,
			Properties:  props,
		})
	}

	return caps
}

// discoverNetwork enumerates network interfaces.
func (d *SystemHardwareDiscoverer) discoverNetwork() []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability {
	var caps []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability

	netPath := "/sys/class/net"
	entries, err := os.ReadDir(netPath)
	if err != nil {
		return nil
	}

	for _, entry := range entries {
		if entry.Name() == "lo" {
			continue
		}

		devPath := filepath.Join(netPath, entry.Name())
		ifType := readSysfsFile(filepath.Join(devPath, "type"))
		operState := readSysfsFile(filepath.Join(devPath, "operstate"))

		name := entry.Name()
		if operState != "" {
			name = fmt.Sprintf("%s (%s)", entry.Name(), operState)
		}
		_ = ifType

		caps = append(caps, &agentpb.ListHardwareCapabilitiesResponse_HardwareCapability{
			Category:    "network",
			DevicePath:  devPath,
			Description: name,
		})
	}

	return caps
}

// discoverStorage enumerates block devices.
func (d *SystemHardwareDiscoverer) discoverStorage() []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability {
	var caps []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability

	blockPath := "/sys/block"
	entries, err := os.ReadDir(blockPath)
	if err != nil {
		return nil
	}

	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "loop") || strings.HasPrefix(entry.Name(), "ram") {
			continue
		}

		devPath := filepath.Join("/dev", entry.Name())
		model := readSysfsFile(filepath.Join(blockPath, entry.Name(), "device", "model"))
		size := readSysfsFile(filepath.Join(blockPath, entry.Name(), "size"))

		name := entry.Name()
		if model != "" {
			name = fmt.Sprintf("%s (%s)", entry.Name(), model)
		}
		_ = size

		caps = append(caps, &agentpb.ListHardwareCapabilitiesResponse_HardwareCapability{
			Category:    "storage",
			DevicePath:  devPath,
			Description: name,
		})
	}

	return caps
}

func readSysfsFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
