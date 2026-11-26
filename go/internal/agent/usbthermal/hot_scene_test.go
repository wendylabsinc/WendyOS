package usbthermal

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"testing"
)

// A whole hot scene must reach raw subscribers even in the 0x80xx count band
// that also resembles grey picture words. Exercise USB reassembly, header
// removal, validation and delivery together, using the captured block layout.
func TestCameraDeliversHotScenes(t *testing.T) {
	for _, celsius := range []float64{150, 200, 240, 550} {
		for _, straddled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%gC/straddled=%v", celsius, straddled), func(t *testing.T) {
				l := tc001Max
				window := blockWindow(t)
				for off := windowMargin + l.PictureBytes(); off < windowMargin+l.RawBytes(); off += 2 {
					binary.LittleEndian.PutUint16(window[off:], celsiusToCounts(celsius))
				}
				want := bytes.Clone(window[windowMargin : windowMargin+l.RawBytes()])
				b := newFakeBus(t)
				b.plugTC001Max("1-2.3", 15)
				dev := newFakeDevice()
				dev.bulk = replayFrames(1, func(seq uint16) []byte {
					w := withCounter(window, seq)
					if straddled {
						return insertHeader(w, l.PictureBytes()+126*l.RowBytes())
					}
					return w
				})
				b.attach(b.node(1, 15), dev)
				m := newTestManager(t, b, nil)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				raw, err := m.SubscribeRaw(ctx, exxonStableID)
				if err != nil {
					t.Fatal(err)
				}
				pics, err := m.SubscribePictures(ctx, exxonStableID)
				if err != nil {
					t.Fatal(err)
				}
				for range 3 {
					if f := receive(t, raw); !bytes.Equal(f.Data, want) {
						t.Fatal("hot scene changed or contains a composite-frame header")
					}
					receive(t, pics)
				}
				if err := m.LastError(exxonStableID); err != nil {
					t.Fatalf("valid hot scene reported an error: %v", err)
				}
			})
		}
	}
}
