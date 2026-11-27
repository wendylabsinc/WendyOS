//go:build !darwin && !linux

package recording

import "errors"

func renameNoReplace(parent int, from, to string) error {
	return errors.New("atomic recording installation is supported only on Darwin and Linux")
}
