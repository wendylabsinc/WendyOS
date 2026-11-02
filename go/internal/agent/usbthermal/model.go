package usbthermal

// Model is one supported camera: how to recognise it on the bus and how its
// thermal frames are laid out.
//
// The table is deliberately keyed by USB vendor and product id. These cameras
// have no UVC descriptors and no kernel driver, so the id pair is the only
// thing the bus says about them; the agent reads the camera's own model string
// once it owns the device and logs it, so a mismatch is visible.
type Model struct {
	VendorID  uint16
	ProductID uint16
	// Name is the camera's own name for itself, as its USB product string and
	// its model register both report it.
	Name string
	// DisplayName is what ListVideoDevices calls it. "(thermal)" because the
	// same housing carries a visible camera this driver does not serve yet.
	DisplayName string
	Layout      Layout
}

// Layout is the geometry of one thermal block as the camera sends it:
//
//	rows 0 .. H-1     the picture: one pixel per two bytes, YUYV with neutral
//	                  chroma (low byte grey level, high byte 0x80)
//	rows H .. 2H-1    temperatures: one little-endian uint16 per pixel, in
//	                  1/64 kelvin
//	row  2H           metadata (counters and calibration words)
//	row  2H+1         end-of-block row: 0xFF 0x00 repeated
//
// Every row is 2*Width bytes. The first 2H rows are exactly the stacked
// 256x384 layout the agent already describes for the TOPDON TC001's own UVC
// mode, which is why the raw tap can serve them unchanged (see RawFrame).
type Layout struct {
	Width  int // sensor pixels per row
	Height int // sensor rows
}

// RowBytes is the length of one row in every part of the block.
func (l Layout) RowBytes() int { return 2 * l.Width }

// PictureBytes is the length of the picture (and, equally, of the
// temperature plane).
func (l Layout) PictureBytes() int { return l.RowBytes() * l.Height }

// RawBytes is the length of the stacked picture-plus-temperature frame the raw
// tap serves: the block without its metadata and end-of-block rows.
func (l Layout) RawBytes() int { return 2 * l.PictureBytes() }

// MetadataOffset is where the metadata row starts within a block.
func (l Layout) MetadataOffset() int { return l.RawBytes() }

// BlockBytes is the length of a whole thermal block as it travels on the bus.
func (l Layout) BlockBytes() int { return l.RawBytes() + 2*l.RowBytes() }

// IDs of the cameras this package knows about.
const (
	VendorRaytron   = 0x3474 // Yantai IRay (InfiRay / Raytron)
	ProductTC001Max = 0x4962
	tc001MaxName    = "TC001 Max"
	tc001MaxDisplay = "TC001 Max (thermal)"
	tc001MaxSensorW = 256
	tc001MaxSensorH = 192
)

// models lists every camera the agent drives from userspace.
//
// The Thermal Master P3 (3474:45a2, 256x192) and P1 (3474:45c2, 160x120) speak
// the same command protocol as the TC001 Max (community-documented, see the
// package comment), and can be added here once one has been measured. They are
// not added blind because their streams differ in ways the parser depends on:
// they send thermal frames only, one frame per bulk transfer between 12-byte
// start and end markers, with the metadata rows BETWEEN the picture and the
// temperatures rather than after them. A row for them needs its own framer,
// not just an id.
var models = []Model{
	{
		VendorID:    VendorRaytron,
		ProductID:   ProductTC001Max,
		Name:        tc001MaxName,
		DisplayName: tc001MaxDisplay,
		Layout:      Layout{Width: tc001MaxSensorW, Height: tc001MaxSensorH},
	},
}

// modelFor returns the supported model with this vendor and product id.
func modelFor(vendor, product uint16) (Model, bool) {
	for _, m := range models {
		if m.VendorID == vendor && m.ProductID == product {
			return m, true
		}
	}
	return Model{}, false
}
