package services

import (
	"io"
	"os"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshname"
)

// FriendlyDeviceName is the device label shared by enrolled-device discovery
// and signed mesh announcements. An explicit rename is literal; the installed
// device name comes next, before the generated OS hostname's wendyos- prefix.
// Names are validated, not stripped or otherwise rewritten into another name.
func FriendlyDeviceName() string {
	hostname, _ := os.Hostname()
	return friendlyDeviceName([]string{explicitHostnamePath, "/etc/wendyos/device-name"}, hostname)
}

func friendlyDeviceName(paths []string, hostname string) string {
	for _, path := range paths {
		if name, ok := readFriendlyDeviceName(path); ok {
			return name
		}
	}
	name, _ := meshname.FriendlyLabel(hostname)
	return name
}

func readFriendlyDeviceName(path string) (string, bool) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64 {
		return "", false
	}
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	data, err := io.ReadAll(io.LimitReader(file, 65))
	if err != nil || len(data) > 64 {
		return "", false
	}
	return meshname.FriendlyLabel(strings.TrimSpace(string(data)))
}
