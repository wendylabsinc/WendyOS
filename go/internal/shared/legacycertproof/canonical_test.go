package legacycertproof

import (
	"encoding/binary"
	"fmt"
)

const domain = "wendy-legacy-certificate-proof/v1"

func canonicalBytes(components ...string) ([]byte, error) {
	result := make([]byte, 0, 256)
	for _, component := range append([]string{domain}, components...) {
		if uint64(len(component)) > uint64(^uint32(0)) {
			return nil, fmt.Errorf("legacy certificate proof component is too large")
		}
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(component)))
		result = append(result, length[:]...)
		result = append(result, component...)
	}
	return result, nil
}
