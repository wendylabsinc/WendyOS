//go:build darwin

package central

import (
	"errors"
	"strings"

	"github.com/google/uuid"
)

func darwinPeripheralAddress(address string) (string, error) {
	id, err := uuid.Parse(address)
	if err != nil || id == uuid.Nil {
		return "", errors.New("macOS BLE requires a CoreBluetooth peripheral UUID, not a hardware MAC address; use an address from wendy device wendy-net discover --transport ble")
	}
	return strings.ToUpper(id.String()), nil
}

// ValidateL2CAPAddress checks the platform-specific address without touching BLE.
func ValidateL2CAPAddress(address string) error {
	_, err := darwinPeripheralAddress(address)
	return err
}
