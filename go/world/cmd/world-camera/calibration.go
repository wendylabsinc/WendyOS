package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/wendylabsinc/wendy/go/world/internal/camerapose"
)

// The binding is an explicit operator declaration, not device attestation.
// camerapose checks its fields against the selected profile and incoming frames.
func readCalibrationBinding(path string) (*camerapose.CalibrationBinding, error) {
	b, err := readRegular(path, 16<<10)
	if err != nil {
		return nil, err
	}
	var binding *camerapose.CalibrationBinding
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&binding); err != nil || binding == nil {
		return nil, errors.New("invalid camera calibration binding")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, errors.New("camera calibration binding has trailing data")
	}
	return binding, nil
}
