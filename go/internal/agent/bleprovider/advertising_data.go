package bleprovider

import (
	"bytes"
	"strings"

	"github.com/google/uuid"
)

// advertisingFields contains only fields from this live report, never Device1
// caches. The UUID in a 128-bit AD field is little-endian; payload fields retain
// their explicitly specified big-endian order.
type advertisingFields struct {
	tenant          bool
	uuidListPresent bool
	servicePresent  bool
	service         []byte
	namePresent     bool
	name            string
	shortened       bool
}

func parseAdvertisingData(data []byte, tenant string) (advertisingFields, error) {
	var out advertisingFields
	id, err := uuid.Parse(tenant)
	if err != nil {
		return out, ErrInvalidAdvertisement
	}
	var wire [16]byte
	for i := range wire {
		wire[i] = id[15-i]
	}
	for len(data) > 0 {
		n := int(data[0])
		data = data[1:]
		if n == 0 {
			// Management reports concatenate advertising and scan-response
			// data. Padding at the end of the first packet can precede more AD.
			continue
		}
		if n > len(data) {
			return out, ErrInvalidAdvertisement
		}
		kind, value := data[0], data[1:n]
		data = data[n:]
		switch kind {
		case 0x06, 0x07: // incomplete/complete list of 128-bit service UUIDs
			out.uuidListPresent = true
			if len(value)%16 != 0 {
				return out, ErrInvalidAdvertisement
			}
			for i := 0; i < len(value); i += 16 {
				if bytes.Equal(value[i:i+16], wire[:]) {
					out.tenant = true
				}
			}
		case 0x21: // service data with 128-bit UUID
			if len(value) < 16 {
				return out, ErrInvalidAdvertisement
			}
			if bytes.Equal(value[:16], wire[:]) {
				if out.servicePresent {
					return out, ErrInvalidAdvertisement
				}
				out.servicePresent = true
				out.service = bytes.Clone(value[16:])
			}
		case 0x08, 0x09: // shortened/complete local name
			if out.namePresent {
				// A primary packet's shortened name and the scan response's
				// complete name can appear together, in either report order.
				if out.shortened && kind == 0x09 && strings.HasPrefix(string(value), out.name) {
					out.name, out.shortened = string(value), false
					continue
				}
				if !out.shortened && kind == 0x08 && strings.HasPrefix(out.name, string(value)) {
					continue
				}
				return out, ErrInvalidAdvertisement
			}
			out.namePresent, out.name, out.shortened = true, string(value), kind == 0x08
		}
	}
	return out, nil
}
