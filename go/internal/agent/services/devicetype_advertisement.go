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
	unlock := LockAvahiServiceFile()
	defer unlock()

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
// has none.
func avahiContentWithDeviceType(content, deviceType string) string {
	return editWendyOSServiceBlock(content, func(block string) string {
		return setAvahiTXTRecord(block, "devicetype", deviceType)
	})
}

// setAvahiTXTRecord sets key=value in a service block, adding the record when
// the block has none.
func setAvahiTXTRecord(block, key, value string) string {
	if strings.Contains(block, "<txt-record>"+key+"=") {
		return replaceAvahiTXTRecord(block, key, value)
	}
	return strings.Replace(block, "</service>",
		"    <txt-record>"+key+"="+value+"</txt-record>\n  </service>", 1)
}

// editWendyOSServiceBlock applies edit to the _wendyos._udp service block of
// content and leaves every other service block alone; content without that
// block comes back unchanged. The block walk mirrors
// configpartition.updateWendyOSServicePort; it lives here because
// configpartition imports this package, not the other way round.
func editWendyOSServiceBlock(content string, edit func(block string) string) string {
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
	return content[:serviceStart] + edit(content[serviceStart:serviceEnd]) + content[serviceEnd:]
}
