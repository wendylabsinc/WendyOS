package appconfig

import (
	"strconv"
	"strings"
)

// EntitlementAnnotationKeyPrefix is the shared prefix for per-entitlement OCI
// manifest annotations and containerd container labels.
const EntitlementAnnotationKeyPrefix = "sh.wendy/entitlement."

// EntitlementAnnotationValue encodes an entitlement's parameters as a
// comma-separated key=value string suitable for an OCI manifest annotation
// or a containerd container label value.
//
// The format is:
//
//	key=value,key=value,...
//
// List-valued fields (pins, allowlist, ports) are folded into their segment
// using a comma-separated list; the parser distinguishes list continuations
// from new keys by checking whether the segment after each comma starts with
// an identifier followed by '='. Entitlements with no parameters produce an
// empty string.
//
// Examples:
//
//	bluetooth                → ""
//	network(mode=host)       → "mode=host"
//	persist(data,/data)      → "name=data,path=/data"
//	gpio(pins=[17,18])       → "pins=17,18"
//	mcp(port=8080)           → "port=8080"
//	network(host,[8080:80])  → "mode=host,ports=8080:80"
func EntitlementAnnotationValue(e Entitlement) string {
	var parts []string
	if e.Mode != "" {
		parts = append(parts, "mode="+e.Mode)
	}
	if e.Name != "" {
		parts = append(parts, "name="+e.Name)
	}
	if e.Path != "" {
		parts = append(parts, "path="+e.Path)
	}
	if e.Device != "" {
		parts = append(parts, "device="+e.Device)
	}
	if e.Port != 0 {
		parts = append(parts, "port="+strconv.Itoa(e.Port))
	}
	if len(e.Pins) > 0 {
		pinStrs := make([]string, len(e.Pins))
		for i, p := range e.Pins {
			pinStrs[i] = strconv.Itoa(p)
		}
		parts = append(parts, "pins="+strings.Join(pinStrs, ","))
	}
	if len(e.Allowlist) > 0 {
		parts = append(parts, "allowlist="+strings.Join(e.Allowlist, ","))
	}
	if len(e.Ports) > 0 {
		pmStrs := make([]string, len(e.Ports))
		for i, pm := range e.Ports {
			pmStrs[i] = strconv.Itoa(int(pm.Host)) + ":" + strconv.Itoa(int(pm.Container))
		}
		parts = append(parts, "ports="+strings.Join(pmStrs, ","))
	}
	return strings.Join(parts, ",")
}

// ParseEntitlementAnnotation decodes a comma-separated key=value string (as
// produced by EntitlementAnnotationValue) into an Entitlement of the given type.
func ParseEntitlementAnnotation(entType, value string) Entitlement {
	ent := Entitlement{Type: entType}
	for _, param := range splitAnnotationParams(value) {
		eq := strings.IndexByte(param, '=')
		if eq < 0 {
			continue
		}
		key := param[:eq]
		val := param[eq+1:]
		switch key {
		case "mode":
			ent.Mode = val
		case "name":
			ent.Name = val
		case "path":
			ent.Path = val
		case "device":
			ent.Device = val
		case "port":
			if n, err := strconv.Atoi(val); err == nil {
				ent.Port = n
			}
		case "pins":
			for _, s := range strings.Split(val, ",") {
				if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
					ent.Pins = append(ent.Pins, n)
				}
			}
		case "allowlist":
			if val != "" {
				ent.Allowlist = strings.Split(val, ",")
			}
		case "ports":
			for _, pm := range strings.Split(val, ",") {
				halves := strings.SplitN(pm, ":", 2)
				if len(halves) != 2 {
					continue
				}
				h, err1 := strconv.ParseUint(halves[0], 10, 16)
				c, err2 := strconv.ParseUint(halves[1], 10, 16)
				if err1 != nil || err2 != nil {
					continue
				}
				ent.Ports = append(ent.Ports, PortMapping{Host: uint16(h), Container: uint16(c)})
			}
		}
	}
	return ent
}

// splitAnnotationParams splits an annotation value on commas that immediately
// precede a new key=value pair (identified by the pattern [a-z][a-z0-9]*=).
// Commas that appear within list values such as "pins=17,18" or
// "ports=8080:80,9090:90" are preserved as part of the preceding segment.
func splitAnnotationParams(s string) []string {
	if s == "" {
		return nil
	}
	var params []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] != ',' {
			continue
		}
		rest := s[i+1:]
		eqIdx := strings.IndexByte(rest, '=')
		if eqIdx > 0 && isAnnotationKey(rest[:eqIdx]) {
			params = append(params, s[start:i])
			start = i + 1
		}
	}
	return append(params, s[start:])
}

// isAnnotationKey reports whether s is a valid annotation parameter name:
// one or more lowercase ASCII letters or digits, starting with a letter.
func isAnnotationKey(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if i == 0 && (c < 'a' || c > 'z') {
			return false
		}
		if i > 0 && !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

// BuildEntitlementAnnotations converts entitlements into a map of
// sh.wendy/entitlement.* keys to their encoded values, suitable for use as OCI
// manifest annotations or containerd container labels. When multiple entitlements
// share the same type a numeric suffix (.0, .1, …) is appended to disambiguate.
// Entitlements with an empty type are skipped.
func BuildEntitlementAnnotations(entitlements []Entitlement) map[string]string {
	typeCounts := make(map[string]int)
	for _, e := range entitlements {
		if e.Type != "" {
			typeCounts[e.Type]++
		}
	}
	typeIndex := make(map[string]int)
	out := make(map[string]string)
	for _, e := range entitlements {
		if e.Type == "" {
			continue
		}
		var key string
		if typeCounts[e.Type] == 1 {
			key = EntitlementAnnotationKeyPrefix + e.Type
		} else {
			key = EntitlementAnnotationKeyPrefix + e.Type + "." + strconv.Itoa(typeIndex[e.Type])
			typeIndex[e.Type]++
		}
		out[key] = EntitlementAnnotationValue(e)
	}
	return out
}
