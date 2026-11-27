//go:build android

// Package androidhost calls the Android application's native system services.
package androidhost

import (
	"encoding/json"
	"errors"
)

// Invoke is installed once by JNI before the backend starts any workers.
var Invoke func([]byte) ([]byte, error)

func Call(method string, args any, out any) error {
	if Invoke == nil {
		return errors.New("Android services are not attached")
	}
	raw, err := json.Marshal(map[string]any{"method": method, "args": args})
	if err != nil {
		return err
	}
	result, err := Invoke(raw)
	if err != nil {
		return err
	}
	var envelope struct {
		Value json.RawMessage `json:"value"`
		Error string          `json:"error"`
	}
	if err = json.Unmarshal(result, &envelope); err != nil {
		return err
	}
	if envelope.Error != "" {
		return errors.New(envelope.Error)
	}
	if out != nil {
		return json.Unmarshal(envelope.Value, out)
	}
	return nil
}
