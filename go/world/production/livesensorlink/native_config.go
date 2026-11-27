package livesensorlink

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/wendylabsinc/wendy/go/world/production/jws"
)

// NativeBridgeConfig selects a host-bound native source through the ordinary
// World socket. The metadata URL supplies only the exact simulation mapping;
// it does not supply camera bytes or replace the native frame timestamp.
type NativeBridgeConfig struct {
	Commission                Commission `json:"commission"`
	Bounds                    Bounds     `json:"bounds"`
	IsolatedSimulationNetwork bool       `json:"isolated_simulation_network"`
	MetadataURL               string     `json:"metadata_url"`
	MetadataReadTokenFile     string     `json:"metadata_read_token_file"`
	ChannelID                 uint32     `json:"channel_id"`
	SourceID                  string     `json:"source_id"`
	PollIntervalMS            int64      `json:"poll_interval_ms,omitempty"`
}

func (c NativeBridgeConfig) normalize() (NativeBridgeConfig, error) {
	var err error
	c.Bounds, err = c.Bounds.normalize()
	if err != nil {
		return c, err
	}
	if c.PollIntervalMS == 0 {
		c.PollIntervalMS = 250
	}
	if !c.IsolatedSimulationNetwork || c.Commission.validate() != nil || !origin(c.MetadataURL) || c.MetadataReadTokenFile == "" || c.ChannelID == 0 || c.PollIntervalMS < 125 || c.PollIntervalMS > 1000 {
		return c, ErrConfig
	}
	if !strings.HasPrefix(c.SourceID, "sensorlink:") || len(c.SourceID) > 256 || !utf8.ValidString(c.SourceID) || !strings.HasSuffix(c.SourceID, ":"+strconv.FormatUint(uint64(c.ChannelID), 10)) {
		return c, ErrConfig
	}
	for _, r := range c.SourceID {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return c, ErrConfig
		}
	}
	if len(strings.TrimSuffix(strings.TrimPrefix(c.SourceID, "sensorlink:"), ":"+strconv.FormatUint(uint64(c.ChannelID), 10))) == 0 {
		return c, ErrConfig
	}
	return c, nil
}

func LoadNativeBridgeConfig(path string) (NativeBridgeConfig, error) {
	var c NativeBridgeConfig
	b, err := readFile(path, 16384, false)
	if err != nil || jws.DecodeStrict(b, &c) != nil {
		return c, ErrConfig
	}
	return c.normalize()
}
