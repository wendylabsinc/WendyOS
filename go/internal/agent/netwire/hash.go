package netwire

import (
	"encoding/hex"
	"errors"
)

func HashBytes(text string) ([]byte, error) {
	bytes, err := hex.DecodeString(text)
	if err != nil || len(bytes) != 32 || hex.EncodeToString(bytes) != text {
		return nil, errors.New("invalid SHA-256 identifier")
	}
	return bytes, nil
}
func HashText(bytes []byte) (string, error) {
	if len(bytes) != 32 {
		return "", errors.New("invalid SHA-256 identifier length")
	}
	return hex.EncodeToString(bytes), nil
}
