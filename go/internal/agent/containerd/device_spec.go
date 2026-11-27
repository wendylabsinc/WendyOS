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

// marshalGrantedAccessSpec appends new mounts raw, so stored ones keep unmodeled
// fields, and replaces the process groups and env.
func marshalGrantedAccessSpec(withDevices []byte, spec *localoci.Spec) ([]byte, error) {
	var document, process, user map[string]json.RawMessage
	if err := json.Unmarshal(withDevices, &document); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(document["process"], &process); err != nil {
		return nil, err
	}
	if process == nil || spec.Process == nil {
		return nil, fmt.Errorf("stored spec has no process")
	}

	var mounts []json.RawMessage
	if raw := document["mounts"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &mounts); err != nil {
			return nil, err
		}
	}
	stored := make(map[string]bool, len(mounts))
	for _, raw := range mounts {
		var m struct {
			Destination string `json:"destination"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		stored[m.Destination] = true
	}
	for _, m := range spec.Mounts {
		if stored[m.Destination] {
			continue
		}
		stored[m.Destination] = true
		raw, err := json.Marshal(m)
		if err != nil {
			return nil, err
		}
		mounts = append(mounts, raw)
	}

	var err error
	if len(mounts) > 0 {
		if document["mounts"], err = json.Marshal(mounts); err != nil {
			return nil, err
		}
	}
	if err := setOrDelete(process, "env", spec.Process.Env, len(spec.Process.Env) == 0); err != nil {
		return nil, err
	}
	if raw := process["user"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &user); err != nil {
			return nil, err
		}
	}
	if user == nil {
		user = make(map[string]json.RawMessage)
	}
	gids := spec.Process.User.AdditionalGids
	if err := setOrDelete(user, "additionalGids", gids, len(gids) == 0); err != nil {
		return nil, err
	}
	if process["user"], err = json.Marshal(user); err != nil {
		return nil, err
	}
	if document["process"], err = json.Marshal(process); err != nil {
		return nil, err
	}
	return json.Marshal(document)
}

func setOrDelete(object map[string]json.RawMessage, key string, value any, empty bool) error {
	if empty {
		delete(object, key)
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	object[key] = raw
	return nil
}
