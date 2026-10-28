package commands

import (
	"testing"

	"github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
)

func TestCloudDiscoverTableRows_OmitsAddress(t *testing.T) {
	devType, ip := "raspberry-pi-5", "192.168.1.50"
	a := &cloudpb.Asset{Id: 1, Name: "alpha", DeviceType: &devType, IpAddress: &ip}
	rows := cloudDiscoverTableRows([]*cloudpb.Asset{a}, nil)
	if len(rows) != 1 {
		t.Fatalf("rows = %d; want 1", len(rows))
	}
	row := rows[0]
	// Columns are: Name, Type, Version — no Address.
	if len(row) != 3 {
		t.Fatalf("row has %d cells; want 3 (Name, Type, Version): %v", len(row), row)
	}
	for _, cell := range row {
		if cell == "192.168.1.50" {
			t.Fatalf("address must be omitted from the cloud discover row: %v", row)
		}
	}
	if row[0] != "alpha" {
		t.Errorf("name cell = %q; want alpha", row[0])
	}
}

func TestDiscoverTableHeaders_OmitAddress(t *testing.T) {
	for _, h := range discoverTableHeaders {
		if h == "Address" {
			t.Fatalf("cloud discover headers should not include Address: %v", discoverTableHeaders)
		}
	}
	if got, want := len(discoverTableHeaders), len(discoverTableMinWidths); got != want {
		t.Fatalf("headers (%d) and min widths (%d) length mismatch", got, want)
	}
	if got, want := len(discoverTableHeaders), len(discoverTableMaxWidths); got != want {
		t.Fatalf("headers (%d) and max widths (%d) length mismatch", got, want)
	}
}
