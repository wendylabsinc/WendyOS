package meshsharing

import (
	"encoding/json"
	"errors"
	"io"
	"os"
)

// LoadConfig accepts the existing nan-mesh.json sharing controls. An absent
// file means disabled; malformed or unknown controls remain explicit errors.
func LoadConfig(path string) (Config, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, err
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return Config{}, err
	}
	if !stat.Mode().IsRegular() || stat.Size() > 16*1024 {
		return Config{}, errors.New("invalid mesh sharing configuration file")
	}
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var config Config
	if err = decoder.Decode(&config); err != nil {
		return Config{}, err
	}
	var extra any
	if err = decoder.Decode(&extra); err != io.EOF {
		return Config{}, errors.New("trailing mesh sharing configuration")
	}
	if err = config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}
