package appconfig

import (
	"reflect"
	"testing"
)

func TestEntitlementAnnotationValue(t *testing.T) {
	tests := []struct {
		name string
		ent  Entitlement
		want string
	}{
		{
			name: "no-param entitlement",
			ent:  Entitlement{Type: EntitlementBluetooth},
			want: "",
		},
		{
			name: "single string param",
			ent:  Entitlement{Type: EntitlementNetwork, Mode: "host"},
			want: "mode=host",
		},
		{
			name: "two string params",
			ent:  Entitlement{Type: EntitlementPersist, Name: "data", Path: "/data"},
			want: "name=data,path=/data",
		},
		{
			name: "int param",
			ent:  Entitlement{Type: EntitlementMCP, Port: 8080},
			want: "port=8080",
		},
		{
			name: "string device param",
			ent:  Entitlement{Type: EntitlementI2C, Device: "i2c-1"},
			want: "device=i2c-1",
		},
		{
			name: "list of ints",
			ent:  Entitlement{Type: EntitlementGPIO, Pins: []int{17, 18, 27}},
			want: "pins=17,18,27",
		},
		{
			name: "list of strings",
			ent:  Entitlement{Type: EntitlementCamera, Allowlist: []string{"/dev/video0", "/dev/video1"}},
			want: "allowlist=/dev/video0,/dev/video1",
		},
		{
			name: "port mappings",
			ent:  Entitlement{Type: EntitlementNetwork, Ports: []PortMapping{{Host: 8080, Container: 80}, {Host: 9090, Container: 90}}},
			want: "ports=8080:80,9090:90",
		},
		{
			name: "mode and port mappings",
			ent:  Entitlement{Type: EntitlementNetwork, Mode: "host", Ports: []PortMapping{{Host: 8080, Container: 80}}},
			want: "mode=host,ports=8080:80",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := EntitlementAnnotationValue(tc.ent)
			if got != tc.want {
				t.Errorf("EntitlementAnnotationValue(%+v) = %q; want %q", tc.ent, got, tc.want)
			}
		})
	}
}

func TestParseEntitlementAnnotation(t *testing.T) {
	tests := []struct {
		name    string
		entType string
		value   string
		want    Entitlement
	}{
		{
			name:    "empty value",
			entType: EntitlementBluetooth,
			value:   "",
			want:    Entitlement{Type: EntitlementBluetooth},
		},
		{
			name:    "mode",
			entType: EntitlementNetwork,
			value:   "mode=host",
			want:    Entitlement{Type: EntitlementNetwork, Mode: "host"},
		},
		{
			name:    "name and path",
			entType: EntitlementPersist,
			value:   "name=data,path=/data",
			want:    Entitlement{Type: EntitlementPersist, Name: "data", Path: "/data"},
		},
		{
			name:    "int port",
			entType: EntitlementMCP,
			value:   "port=8080",
			want:    Entitlement{Type: EntitlementMCP, Port: 8080},
		},
		{
			name:    "device",
			entType: EntitlementI2C,
			value:   "device=i2c-1",
			want:    Entitlement{Type: EntitlementI2C, Device: "i2c-1"},
		},
		{
			name:    "pins list",
			entType: EntitlementGPIO,
			value:   "pins=17,18,27",
			want:    Entitlement{Type: EntitlementGPIO, Pins: []int{17, 18, 27}},
		},
		{
			name:    "allowlist",
			entType: EntitlementCamera,
			value:   "allowlist=/dev/video0,/dev/video1",
			want:    Entitlement{Type: EntitlementCamera, Allowlist: []string{"/dev/video0", "/dev/video1"}},
		},
		{
			name:    "port mappings",
			entType: EntitlementNetwork,
			value:   "ports=8080:80,9090:90",
			want:    Entitlement{Type: EntitlementNetwork, Ports: []PortMapping{{Host: 8080, Container: 80}, {Host: 9090, Container: 90}}},
		},
		{
			name:    "mode and port mappings",
			entType: EntitlementNetwork,
			value:   "mode=host,ports=8080:80",
			want:    Entitlement{Type: EntitlementNetwork, Mode: "host", Ports: []PortMapping{{Host: 8080, Container: 80}}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseEntitlementAnnotation(tc.entType, tc.value)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseEntitlementAnnotation(%q, %q) = %+v; want %+v", tc.entType, tc.value, got, tc.want)
			}
		})
	}
}

func TestEntitlementAnnotationRoundTrip(t *testing.T) {
	entitlements := []Entitlement{
		{Type: EntitlementBluetooth},
		{Type: EntitlementGPU},
		{Type: EntitlementNetwork, Mode: "host"},
		{Type: EntitlementNetwork, Mode: "host", Ports: []PortMapping{{Host: 8080, Container: 80}, {Host: 9090, Container: 90}}},
		{Type: EntitlementPersist, Name: "data", Path: "/data"},
		{Type: EntitlementCamera, Mode: "detect", Allowlist: []string{"/dev/video0", "/dev/video1"}},
		{Type: EntitlementGPIO, Pins: []int{17, 18, 27}},
		{Type: EntitlementMCP, Port: 8080},
		{Type: EntitlementI2C, Device: "i2c-1"},
	}

	for _, want := range entitlements {
		value := EntitlementAnnotationValue(want)
		got := ParseEntitlementAnnotation(want.Type, value)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("round-trip %+v: encoded %q, decoded %+v", want, value, got)
		}
	}
}

func TestSplitAnnotationParams(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"", nil},
		{"mode=host", []string{"mode=host"}},
		{"mode=host,ports=8080:80,9090:90", []string{"mode=host", "ports=8080:80,9090:90"}},
		{"pins=17,18,27", []string{"pins=17,18,27"}},
		{"name=data,path=/data", []string{"name=data", "path=/data"}},
		{"allowlist=/dev/video0,/dev/video1", []string{"allowlist=/dev/video0,/dev/video1"}},
		{"mode=detect,allowlist=/dev/video0,/dev/video1", []string{"mode=detect", "allowlist=/dev/video0,/dev/video1"}},
	}

	for _, tc := range tests {
		got := splitAnnotationParams(tc.input)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("splitAnnotationParams(%q) = %v; want %v", tc.input, got, tc.want)
		}
	}
}
