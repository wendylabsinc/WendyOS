//go:build !linux

package services

import "errors"

func writeJetsonEFIVariable(string, []byte) error {
	return errors.New("EFI variable writes require Linux")
}
