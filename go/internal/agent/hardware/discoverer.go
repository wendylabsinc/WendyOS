// Package hardware implements system hardware discovery by reading sysfs, /dev, and /proc.
package hardware

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// SystemHardwareDiscoverer discovers hardware by probing the Linux sysfs/devfs/procfs.
type SystemHardwareDiscoverer struct {
	logger *zap.Logger
}

// NewSystemHardwareDiscoverer creates a new SystemHardwareDiscoverer.
func NewSystemHardwareDiscoverer(logger *zap.Logger) *SystemHardwareDiscoverer {
	return &SystemHardwareDiscoverer{logger: logger}
}

// Discover probes the system for hardware capabilities, optionally filtered by category.
func (d *SystemHardwareDiscoverer) Discover(_ context.Context, categoryFilter string) ([]*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability, error) {
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
		{"camera", d.discoverCamera},
		{"audio", d.discoverAudio},
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

// discoverGPU checks for NVIDIA and DRM GPU devices.
func (d *SystemHardwareDiscoverer) discoverGPU() []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability {
	var caps []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability

	// NVIDIA devices.
	for i := 0; i < 16; i++ {
		path := fmt.Sprintf("/dev/nvidia%d", i)
		if _, err := os.Stat(path); err == nil {
			caps = append(caps, &agentpb.ListHardwareCapabilitiesResponse_HardwareCapability{
				Category:    "gpu",
				DevicePath:  path,
				Description: fmt.Sprintf("NVIDIA GPU %d", i),
			})
		}
	}

	// DRM devices.
	drmPath := "/sys/class/drm"
	entries, err := os.ReadDir(drmPath)
	if err == nil {
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "card") && !strings.Contains(entry.Name(), "-") {
				devPath := filepath.Join("/dev/dri", entry.Name())
				name := entry.Name()

				// Try to read device model.
				labelPath := filepath.Join(drmPath, entry.Name(), "device", "label")
				if data, err := os.ReadFile(labelPath); err == nil {
					name = strings.TrimSpace(string(data))
				}

				caps = append(caps, &agentpb.ListHardwareCapabilitiesResponse_HardwareCapability{
					Category:    "gpu",
					DevicePath:  devPath,
					Description: name,
				})
			}
		}
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

// discoverCamera enumerates V4L2 video devices.
func (d *SystemHardwareDiscoverer) discoverCamera() []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability {
	var caps []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability

	entries, err := filepath.Glob("/dev/video*")
	if err != nil {
		return nil
	}

	for _, path := range entries {
		name := filepath.Base(path)

		// Try to read the device name from sysfs.
		devNum := strings.TrimPrefix(name, "video")
		sysName := filepath.Join("/sys/class/video4linux", name, "name")
		if data, err := os.ReadFile(sysName); err == nil {
			name = fmt.Sprintf("%s (%s)", strings.TrimSpace(string(data)), "video"+devNum)
		}

		caps = append(caps, &agentpb.ListHardwareCapabilitiesResponse_HardwareCapability{
			Category:    "camera",
			DevicePath:  path,
			Description: name,
		})
	}

	return caps
}

// discoverAudio enumerates audio devices from /proc/asound or PipeWire.
func (d *SystemHardwareDiscoverer) discoverAudio() []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability {
	var caps []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability

	f, err := os.Open("/proc/asound/cards")
	if err != nil {
		return nil
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Lines look like: " 0 [tegrahda       ]: tegra-hda - NVIDIA Tegra HDA"
		if len(line) == 0 || line[0] < '0' || line[0] > '9' {
			// Skip description lines (second line of each card).
			continue
		}

		parts := strings.SplitN(line, ":", 2)
		if len(parts) < 2 {
			continue
		}

		name := strings.TrimSpace(parts[1])
		cardParts := strings.Fields(parts[0])
		cardNum := "0"
		if len(cardParts) > 0 {
			cardNum = cardParts[0]
		}

		caps = append(caps, &agentpb.ListHardwareCapabilitiesResponse_HardwareCapability{
			Category:    "audio",
			DevicePath:  fmt.Sprintf("/dev/snd/controlC%s", cardNum),
			Description: name,
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

// readSysfsFile reads a sysfs file and returns its trimmed content.
func readSysfsFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
