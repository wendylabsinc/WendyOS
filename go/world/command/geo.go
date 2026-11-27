package command

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/geography"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
)

const geoSiteLimit = 1 << 20
const geoRequestLimit = 64 << 10

func readGeoFile(path string, limit int) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() {
		return nil, errors.New("geographic input must be a regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > limit {
		return nil, fmt.Errorf("geographic input exceeds %d bytes", limit)
	}
	return b, nil
}

// Exact keys prevent Go's case-insensitive JSON field matching from admitting
// ambiguous spellings. Every field is required, even when its value is zero.
func geoObject(raw []byte, keys ...string) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := authority.DecodeStrictLimit(raw, &object, geoRequestLimit); err != nil {
		return nil, err
	}
	if object == nil || len(object) != len(keys) {
		return nil, errors.New("geographic object has missing or unknown fields")
	}
	for _, key := range keys {
		value, ok := object[key]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, fmt.Errorf("geographic field %s is required and cannot be null", key)
		}
	}
	return object, nil
}

func decodeFrameRequest(raw []byte, locate bool) (geography.Request, geography.GeoRequest, error) {
	var local geography.Request
	var geographic geography.GeoRequest
	keys := []string{"from", "to", "position_m", "at", "uncertainty_metres"}
	if locate {
		keys = []string{"geographic", "to", "at", "uncertainty_metres"}
	}
	object, err := geoObject(raw, keys...)
	if err != nil {
		return local, geographic, err
	}
	for _, key := range []string{"from", "to"} {
		if key == "from" && locate {
			continue
		}
		if _, err = geoObject(object[key], "id", "revision"); err != nil {
			return local, geographic, fmt.Errorf("%s: %w", key, err)
		}
	}
	if locate {
		if _, err = geoObject(object["geographic"], "latitude_degrees", "longitude_degrees", "altitude_metres", "altitude_datum"); err != nil {
			return local, geographic, err
		}
		err = authority.DecodeStrictLimit(raw, &geographic, geoRequestLimit)
	} else {
		var point []*float64
		if err = json.Unmarshal(object["position_m"], &point); err != nil || len(point) != 3 {
			return local, geographic, errors.New("position_m requires exactly three numbers")
		}
		for _, value := range point {
			if value == nil {
				return local, geographic, errors.New("position_m coordinates cannot be null")
			}
		}
		err = authority.DecodeStrictLimit(raw, &local, geoRequestLimit)
	}
	return local, geographic, err
}

func (inv *invocation) runFrame(args []string, output io.Writer) error {
	if len(args) == 0 || (args[0] != "convert" && args[0] != "locate" && args[0] != "project") {
		return errors.New("usage: world frame convert|locate --site FILE --request FILE --json; world frame project --site FILE --sample FILE --at RFC3339 --json")
	}
	flags := flag.NewFlagSet("frame "+args[0], flag.ContinueOnError)
	flags.SetOutput(inv.stderr)
	sitePath := flags.String("site", "", "georeference document")
	requestPath := flags.String("request", "", "explicit coordinate conversion request")
	samplePath := flags.String("sample", "", "typed sensor sample to project")
	atString := flags.String("at", "", "capture time in RFC3339, including timezone")
	jsonOutput := flags.Bool("json", true, "emit JSON")
	if err := inv.parseFlags(flags, args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *sitePath == "" || !*jsonOutput {
		return errors.New("frame conversion requires --site FILE and JSON output; positional arguments are not accepted")
	}
	if args[0] == "project" {
		if *samplePath == "" || *atString == "" || *requestPath != "" {
			return errors.New("frame project requires --sample FILE --at RFC3339 and does not accept --request")
		}
	} else if *requestPath == "" || *samplePath != "" || *atString != "" {
		return errors.New("frame convert/locate requires --request FILE and does not accept --sample or --at")
	}
	siteRaw, err := readGeoFile(*sitePath, geoSiteLimit)
	if err != nil {
		return fmt.Errorf("site: %w", err)
	}
	site, err := geography.DecodeSite(siteRaw)
	if err != nil {
		return fmt.Errorf("site: %w", err)
	}
	if args[0] == "project" {
		at, err := time.Parse(time.RFC3339Nano, *atString)
		if err != nil || at.IsZero() {
			return errors.New("--at must be a nonzero RFC3339 capture time with timezone")
		}
		raw, err := readGeoFile(*samplePath, geoRequestLimit)
		if err != nil {
			return fmt.Errorf("sample: %w", err)
		}
		sample, err := sensing.Decode(raw)
		if err != nil {
			return fmt.Errorf("sample: %w", err)
		}
		result, err := site.ProjectSample(sample, at)
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(result)
	}
	requestRaw, err := readGeoFile(*requestPath, geoRequestLimit)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	local, geographic, err := decodeFrameRequest(requestRaw, args[0] == "locate")
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	var result geography.Result
	if args[0] == "locate" {
		result, err = site.Geographic(geographic)
	} else {
		result, err = site.Convert(local)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(result)
}
