package services

import (
	"os"
	"regexp"
	"strings"

	"go.uber.org/zap"
)

// deviceTypePath is where the image records the board this agent runs on
// (BOARD=..., or a bare board id on older images).
const deviceTypePath = "/etc/wendyos/device-type"

// advertisableDeviceType bounds what goes into the TXT record: board ids are
// short lowercase tokens, and anything else would be a malformed file's
// content being written into XML.
var advertisableDeviceType = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// EnsureDeviceTypeAdvertisement stamps the board id into the mDNS
// advertisement as a devicetype TXT record. Call it once at agent startup.
//
// The record lets `wendy discover` classify a sighting before -- or instead
// of -- reaching the agent. That matters for the local VM: its announcement
// escapes QEMU's user-mode network one way, carrying an address the host
// cannot dial, and the CLI has to know it is a VM without a probe.
//
// Best-effort and quiet by design: when the record already says the right
// thing it touches nothing and leaves avahi-daemon alone -- this runs on every
// agent start, and a needless restart would drop the advertisement.
func EnsureDeviceTypeAdvertisement(logger *zap.Logger) {
	ensureDeviceTypeAdvertisement(logger, deviceTypePath, avahiServiceFile, func() bool {
		return restartAvahiDaemon(logger)
	})
}

// ensureDeviceTypeAdvertisement is the testable core of
// EnsureDeviceTypeAdvertisement, with the paths and the avahi restart injected.
func ensureDeviceTypeAdvertisement(logger *zap.Logger, deviceTypePath, serviceFile string, restartAvahi func() bool) {
	data, err := os.ReadFile(deviceTypePath)
	if err != nil {
		// Absent on an image that never wrote one, and on desktop installs.
		if !os.IsNotExist(err) {
			logger.Warn("Could not read device type", zap.String("path", deviceTypePath), zap.Error(err))
		}
		return
	}
	deviceType, _ := parseDeviceType(string(data))
	if !advertisableDeviceType.MatchString(deviceType) {
		return
	}

	service, err := os.ReadFile(serviceFile)
	if err != nil {
		// Absent on a non-WendyOS host, where there is no advertisement.
		if !os.IsNotExist(err) {
			logger.Warn("Could not read avahi service file", zap.String("path", serviceFile), zap.Error(err))
		}
		return
	}

	content := avahiContentWithDeviceType(string(service), deviceType)
	if content == string(service) {
		return // already advertising it
	}
	if err := os.WriteFile(serviceFile, []byte(content), 0o644); err != nil {
		logger.Warn("Could not write avahi service file", zap.String("path", serviceFile), zap.Error(err))
		return
	}
	if restartAvahi() {
		logger.Info("Advertising device type over mDNS", zap.String("deviceType", deviceType))
	}
}

// avahiContentWithDeviceType returns content with the devicetype TXT record of
// the _wendyos._udp block set to deviceType, adding the record when the block
// has none. Other service blocks in the file are left alone. The block walk
// mirrors configpartition.updateWendyOSServicePort; it lives here because
// configpartition imports this package, not the other way round.
func avahiContentWithDeviceType(content, deviceType string) string {
	const typeTag = "<type>_wendyos._udp</type>"
	typeIdx := strings.Index(content, typeTag)
	if typeIdx < 0 {
		return content
	}
	serviceStart := strings.LastIndex(content[:typeIdx], "<service")
	if serviceStart < 0 {
		serviceStart = typeIdx
	}
	closeOffset := strings.Index(content[typeIdx:], "</service>")
	if closeOffset < 0 {
		return content
	}
	serviceEnd := typeIdx + closeOffset + len("</service>")

	block := content[serviceStart:serviceEnd]
	if strings.Contains(block, "<txt-record>devicetype=") {
		block = replaceAvahiTXTRecord(block, "devicetype", deviceType)
	} else {
		block = strings.Replace(block, "</service>",
			"    <txt-record>devicetype="+deviceType+"</txt-record>\n  </service>", 1)
	}
	return content[:serviceStart] + block + content[serviceEnd:]
}
