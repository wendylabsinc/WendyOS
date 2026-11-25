//go:build !darwin && !linux

package worldpackage

import "errors"

func renameNoReplace(parent int, from, to string) error {
	return errors.New("atomic package installation is supported only on Darwin and Linux")
}
