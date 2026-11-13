// Package unitree connects to a Go2 using the Unitree mobile app LAN protocol.
package unitree

import "encoding/hex"

// Device identifies a robot. EncryptionKey must not appear in public output.
type Device struct {
	Serial        string `json:"serial"`
	Name          string `json:"name"`
	Address       string `json:"address,omitempty"`
	Model         string `json:"model,omitempty"`
	EncryptionKey string `json:"-"`
}

// Account contains the account-bound robots, never the password or cloud
// bearer token. The token is used only inside the CLI vendor-cloud client.
type Account struct {
	Devices []Device `json:"devices"`
}

// AccountFlowMarker is the only non-empty value for PairUnitreeRequest.token.
// It marks initial keyless pairing after account login; it is not a credential.
const AccountFlowMarker = "unitree-account-flow"

// ValidEncryptionKey accepts the vendor's 16-byte hexadecimal robot key.
func ValidEncryptionKey(key string) bool {
	decoded, err := hex.DecodeString(key)
	return err == nil && len(decoded) == 16
}

// ValidSerial accepts a nonempty, bounded printable-ASCII robot identifier.
// Use the same constraint before discovery, connection setup and persistence.
func ValidSerial(serial string) bool {
	if len(serial) == 0 || len(serial) > 128 {
		return false
	}
	for _, r := range serial {
		if r < '!' || r > '~' {
			return false
		}
	}
	return true
}
