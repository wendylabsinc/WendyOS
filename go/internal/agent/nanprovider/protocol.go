package nanprovider

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Identity is the enrolled organization and asset advertised during NAN
// discovery. The untrusted presence claim is pinned by peer mTLS later.
type Identity struct{ Org, Asset int32 }

func parseEvent(raw string) (string, map[string]string) {
	if i := strings.Index(raw, "NAN-"); i >= 0 {
		raw = raw[i:]
	}
	fields := strings.Fields(raw)
	values := map[string]string{}
	if len(fields) == 0 {
		return "", values
	}
	for _, field := range fields[1:] {
		k, v, ok := strings.Cut(field, "=")
		if ok {
			values[k] = v
		}
	}
	return fields[0], values
}
func decodePresence(ssi string, org int32) (int32, error) {
	b, err := hex.DecodeString(ssi)
	if err != nil {
		return 0, err
	}
	var version int
	var peerOrg, asset int32
	if _, err = fmt.Sscanf(string(b), "wendy-nan:%d:%d:%d", &version, &peerOrg, &asset); err != nil || version != 1 || org != peerOrg || asset <= 0 || asset > 65534 {
		return 0, errors.New("invalid peer presence")
	}
	if string(b) != fmt.Sprintf("wendy-nan:1:%d:%d", org, asset) {
		return 0, errors.New("noncanonical peer presence")
	}
	return asset, nil
}
