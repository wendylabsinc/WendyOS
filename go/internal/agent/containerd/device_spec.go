package containerd

import (
	"encoding/json"
	"fmt"
	localoci "github.com/wendylabsinc/wendy/go/internal/agent/oci"
)

// Change only device fields and pin annotations. Decoding the whole record into
// localoci.Spec would drop runtime fields that its reduced schema cannot model
// (including fields written by older/newer agents). RawMessage also preserves
// large numeric resource limits without float64 conversion.
func marshalRefreshedDeviceSpec(original []byte, spec *localoci.Spec) ([]byte, error) {
	var document, linux, resources map[string]json.RawMessage
	if err := json.Unmarshal(original, &document); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(document["linux"], &linux); err != nil {
		return nil, err
	}
	if linux == nil || spec.Linux == nil {
		return nil, fmt.Errorf("stored spec has no Linux configuration")
	}
	var err error
	linux["devices"], err = json.Marshal(spec.Linux.Devices)
	if err != nil {
		return nil, err
	}
	if spec.Linux.Resources != nil {
		if raw := linux["resources"]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &resources); err != nil {
				return nil, err
			}
		}
		if resources == nil {
			resources = make(map[string]json.RawMessage)
		}
		resources["devices"], err = json.Marshal(spec.Linux.Resources.Devices)
		if err != nil {
			return nil, err
		}
		linux["resources"], err = json.Marshal(resources)
		if err != nil {
			return nil, err
		}
	}
	document["linux"], err = json.Marshal(linux)
	if err != nil {
		return nil, err
	}
	document["annotations"], err = json.Marshal(spec.Annotations)
	if err != nil {
		return nil, err
	}
	return json.Marshal(document)
}
