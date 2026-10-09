package chat

import "encoding/json"

// UntrustedJSONBlock wraps v in an untrusted_sensor_event_json tag, encoded as
// JSON and then encoded again as a JSON string. The second encoding escapes
// every quote and newline, and JSON escapes '<' and '>', so nothing in v can
// close the tag. Prompts that use it say the block is data, not instructions.
func UntrustedJSONBlock(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		data = []byte("null")
	}
	escaped, _ := json.Marshal(string(data))
	return "<untrusted_sensor_event_json>\n" + string(escaped) + "\n</untrusted_sensor_event_json>"
}
