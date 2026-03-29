package discovery

import "strings"

// mdnsEntryMatchesServiceType checks whether an mDNS entry name belongs to the
// queried DNS-SD service type. hashicorp/mdns returns full service instance
// names, so the service type must appear immediately after the instance label.
func mdnsEntryMatchesServiceType(entryName, serviceType string) bool {
	entryLabels := splitDNSSDLabels(strings.Trim(strings.ToLower(entryName), "."))
	serviceLabels := splitDNSSDLabels(strings.Trim(strings.ToLower(serviceType), "."))
	if len(serviceLabels) == 0 || len(entryLabels) < len(serviceLabels)+1 {
		return false
	}

	for i, label := range serviceLabels {
		if entryLabels[i+1] != label {
			return false
		}
	}

	return true
}

func splitDNSSDLabels(name string) []string {
	if name == "" {
		return nil
	}

	labels := make([]string, 0, strings.Count(name, ".")+1)
	var current strings.Builder
	escaped := false

	for _, r := range name {
		switch {
		case escaped:
			current.WriteRune(r)
			escaped = false
		case r == '\\':
			escaped = true
		case r == '.':
			labels = append(labels, current.String())
			current.Reset()
		default:
			current.WriteRune(r)
		}
	}

	if escaped {
		current.WriteByte('\\')
	}

	labels = append(labels, current.String())
	return labels
}
