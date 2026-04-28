// Partition layout XML parsing — translated from NVIDIA tegraflash XML format.
// The partition XML is included in the tegraflash tarball (e.g. flash_t234_qspi_sdmmc.xml).
package bundle

import (
	"encoding/xml"
	"strconv"
	"strings"
)

// xmlInt64 is an int64 that also accepts hex literals (e.g. 0xFFFFFFFFFFFFFFFF)
// in XML text content. NVIDIA uses 0xFFFFFFFFFFFFFFFF as a "fill remaining" sentinel.
type xmlInt64 int64

func (x *xmlInt64) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	var s string
	if err := d.DecodeElement(&s, &start); err != nil {
		return err
	}
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		v, err := strconv.ParseUint(s, 0, 64)
		if err != nil {
			return err
		}
		*x = xmlInt64(int64(v))
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}
	*x = xmlInt64(v)
	return nil
}

// PartitionLayout is the top-level structure of a tegraflash partition XML.
type PartitionLayout struct {
	XMLName xml.Name       `xml:"partition_layout"`
	Devices []DeviceLayout `xml:"device"`
}

// DeviceLayout describes a storage device and its partitions.
type DeviceLayout struct {
	Type       string      `xml:"type,attr"`
	Instance   int         `xml:"instance,attr"`
	SectorSize int         `xml:"sector_size,attr"`
	NumSectors int64       `xml:"num_sectors,attr"`
	Partitions []Partition `xml:"partition"`
}

// IsQSPI reports whether this device is the QSPI NOR flash.
func (d *DeviceLayout) IsQSPI() bool {
	return d.Type == "spi" || d.Type == "qspi"
}

// IsEMMC reports whether this device is the onboard eMMC.
func (d *DeviceLayout) IsEMMC() bool {
	return strings.HasPrefix(d.Type, "sdmmc")
}

// Partition is a single partition entry in the layout XML.
type Partition struct {
	Name     string   `xml:"name,attr"`
	ID       int      `xml:"id,attr"`
	Type     string   `xml:"type,attr"`
	Size     xmlInt64 `xml:"size"`
	Filename string   `xml:"filename"`
}

// HasFile reports whether this partition has a file to write.
func (p *Partition) HasFile() bool {
	return strings.TrimSpace(p.Filename) != ""
}

// IsBCT reports whether this partition is a Boot Configuration Table.
func (p *Partition) IsBCT() bool {
	return p.Type == "boot_config_table" || strings.EqualFold(p.Name, "BCT")
}

// ParseLayout parses a tegraflash partition layout XML.
func ParseLayout(data []byte) (*PartitionLayout, error) {
	var layout PartitionLayout
	if err := xml.Unmarshal(data, &layout); err != nil {
		return nil, err
	}
	return &layout, nil
}
