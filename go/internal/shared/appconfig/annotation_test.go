package appconfig

import (
	"reflect"
	"strconv"
	"strings"
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
		{
			name: "mesh mode with serviceCIDR",
			ent:  Entitlement{Type: EntitlementNetwork, Mode: "mesh", ServiceCIDR: "10.99.0.0/16"},
			want: "mode=mesh,servicecidr=10.99.0.0/16",
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
		{
			name:    "mesh mode with serviceCIDR",
			entType: EntitlementNetwork,
			value:   "mode=mesh,servicecidr=10.99.0.0/16",
			want:    Entitlement{Type: EntitlementNetwork, Mode: "mesh", ServiceCIDR: "10.99.0.0/16"},
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
		{Type: EntitlementSerial, Device: "ttyUSB0"},
		{Type: EntitlementNetwork, Mode: "mesh", ServiceCIDR: "10.99.0.0/16"},
	}

	for _, want := range entitlements {
		value := EntitlementAnnotationValue(want)
		got := ParseEntitlementAnnotation(want.Type, value)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("round-trip %+v: encoded %q, decoded %+v", want, value, got)
		}
	}
}

func TestBuildEntitlementAnnotationsIndexesDistinctSerialDevices(t *testing.T) {
	want := []Entitlement{
		{Type: EntitlementSerial, Device: "ttyUSB0"},
		{Type: EntitlementSerial, Device: "ttyUSB1"},
	}
	annotations := BuildEntitlementAnnotations(want)
	if len(annotations) != 2 {
		t.Fatalf("got %d annotations, want 2: %v", len(annotations), annotations)
	}
	for i, entitlement := range want {
		key := EntitlementAnnotationKeyPrefix + EntitlementSerial + "." + strconv.Itoa(i)
		value, ok := annotations[key]
		if !ok {
			t.Fatalf("missing indexed annotation %q in %v", key, annotations)
		}
		if got := ParseEntitlementAnnotation(EntitlementSerial, value); !reflect.DeepEqual(got, entitlement) {
			t.Errorf("%s round-trip = %+v, want %+v", key, got, entitlement)
		}
	}
}

func TestBuildEntitlementAnnotationsKeepsParameterlessEntitlement(t *testing.T) {
	key := EntitlementAnnotationKeyPrefix + EntitlementCamera
	annotations := BuildEntitlementAnnotations([]Entitlement{{Type: EntitlementCamera}})
	value, ok := annotations[key]
	if !ok {
		t.Fatalf("missing parameterless entitlement annotation %q", key)
	}
	if value == "" {
		t.Fatal("parameterless entitlement annotation has an empty value, which containerd drops")
	}
	if got := ParseEntitlementAnnotation(EntitlementCamera, value); !reflect.DeepEqual(got, Entitlement{Type: EntitlementCamera}) {
		t.Fatalf("round-trip = %+v, want camera entitlement", got)
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

// TestValidateRejectsAllowlistDelimiters covers the codec assumption that
// EntitlementAnnotationValue makes and cannot itself enforce: an entitlement
// travels to the device as a container label of comma-separated key=value
// pairs, with the allowlist folded into one segment by joining on commas. An
// entry containing a comma is silently split into two entries, and one
// containing ",key=" terminates the allowlist and overwrites a sibling field.
// Validation is where an author can still be told, so it is where the two
// characters are refused.
func TestValidateRejectsAllowlistDelimiters(t *testing.T) {
	cases := []struct {
		name  string
		entry string
		// corrupts records whether today's codec actually mangles the entry. A
		// comma does; a bare '=' does not, and is refused only because it is
		// the pair separator itself (see validateAllowlistEntries).
		corrupts bool
	}{
		{"comma splits the entry in two", "/dev/video0,/dev/video1", true},
		{"comma plus key overwrites a sibling field", "/dev/video0,mode=host", true},
		{"bare equals", "name=/dev/video0", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &AppConfig{
				AppID:        "com.example.app",
				Entitlements: []Entitlement{{Type: EntitlementCamera, Mode: "detect", Allowlist: []string{tc.entry}}},
			}
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate() accepted allowlist entry %q", tc.entry)
			}
			if !strings.Contains(err.Error(), "allowlist[0]") {
				t.Fatalf("error does not name the offending entry: %v", err)
			}

			// Show what the codec would have done with it, so the test states
			// the harm rather than only the rule.
			round := ParseEntitlementAnnotation(EntitlementCamera, EntitlementAnnotationValue(cfg.Entitlements[0]))
			intact := len(round.Allowlist) == 1 && round.Allowlist[0] == tc.entry && round.Mode == "detect"
			if tc.corrupts && intact {
				t.Fatalf("entry %q survives the codec intact; the rule may no longer be needed", tc.entry)
			}
			if !tc.corrupts && !intact {
				t.Fatalf("entry %q now corrupts the codec; validateAllowlistEntries should say so", tc.entry)
			}
		})
	}
}

// TestValidateAcceptsOrdinaryAllowlistEntries confirms the rule rejects nothing
// that any shipped source identifier grammar produces.
func TestValidateAcceptsOrdinaryAllowlistEntries(t *testing.T) {
	cfg := &AppConfig{
		AppID: "com.example.app",
		Entitlements: []Entitlement{{
			Type:      EntitlementCamera,
			Mode:      "detect",
			Allowlist: []string{"/dev/video0", "/dev/video1", "v4l2:/dev/video2", "usb-046d_C920-video-index0"},
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() rejected ordinary allowlist entries: %v", err)
	}
}
