package logfields

import "testing"

func TestKeysAreSnakeCaseAndStable(t *testing.T) {
	cases := map[string]string{
		"AppID":         AppID,
		"AppName":       AppName,
		"ContainerID":   ContainerID,
		"ContainerName": ContainerName,
		"ServiceName":   ServiceName,

		"Adapter":                       Adapter,
		"LinkType":                      LinkType,
		"ReasonCode":                    ReasonCode,
		"SupervisionTimeoutMS":          SupervisionTimeoutMS,
		"RequestedSupervisionTimeoutMS": RequestedSupervisionTimeoutMS,
	}
	want := map[string]string{
		"AppID":         "app_id",
		"AppName":       "app_name",
		"ContainerID":   "container_id",
		"ContainerName": "container_name",
		"ServiceName":   "service_name",

		"Adapter":                       "adapter",
		"LinkType":                      "link_type",
		"ReasonCode":                    "reason_code",
		"SupervisionTimeoutMS":          "supervision_timeout_ms",
		"RequestedSupervisionTimeoutMS": "requested_supervision_timeout_ms",
	}
	for name, got := range cases {
		if got != want[name] {
			t.Errorf("%s = %q, want %q", name, got, want[name])
		}
	}
}
