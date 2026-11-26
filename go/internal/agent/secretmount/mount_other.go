//go:build !linux

package secretmount

import "errors"

const Root = "/run/wendy/secrets"

func Supported() bool { return false }
func Prepare(map[string][]byte, uint32, uint32) (string, error) {
	return "", errors.New("app secret mounts require Linux")
}
func Cleanup(path string) error {
	if path == "" {
		return nil
	}
	return errors.New("app secret mounts require Linux")
}
