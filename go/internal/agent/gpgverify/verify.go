package gpgverify

import (
	"bytes"
	"fmt"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
)

// VerifyBinary checks that sig is a valid detached armored GPG signature of
// data, signed by any key in pubKeyArmor. Returns an error if verification
// fails or if sig is empty.
func VerifyBinary(data, sig, pubKeyArmor []byte) error {
	if len(sig) == 0 {
		return fmt.Errorf("signature is empty")
	}

	block, err := armor.Decode(bytes.NewReader(pubKeyArmor))
	if err != nil {
		return fmt.Errorf("decoding public key armor: %w", err)
	}

	keyring, err := openpgp.ReadKeyRing(block.Body)
	if err != nil {
		return fmt.Errorf("reading keyring: %w", err)
	}

	sigBlock, err := armor.Decode(bytes.NewReader(sig))
	if err != nil {
		return fmt.Errorf("decoding signature armor: %w", err)
	}

	_, err = openpgp.CheckDetachedSignature(keyring, bytes.NewReader(data), sigBlock.Body, nil)
	if err != nil {
		return fmt.Errorf("signature verification failed: %w", err)
	}

	return nil
}
