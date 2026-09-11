package tui

import "testing"

// A LAN row the discovery engine takes back (it turned out to be a local VM)
// has to leave the picker, keyed exactly as it was added and case-insensitively
// like every other key comparison here. A key nobody holds is a no-op.
func TestPickerRemoveMsgDropsTheRowByDedupKey(t *testing.T) {
	m := NewPicker()
	updated, _ := m.Update(PickerAddMsg{Items: []PickerItem{
		{Name: "Gentle Forest", DedupKey: "wendyos-gentle-forest"},
		{Name: "orin", DedupKey: "orin"},
	}})
	m = updated.(PickerModel)

	updated, _ = m.Update(PickerRemoveMsg{Key: "WENDYOS-gentle-forest"})
	m = updated.(PickerModel)
	if len(m.items) != 1 || m.items[0].Name != "orin" {
		t.Fatalf("items after remove = %+v, want only orin", m.items)
	}

	updated, _ = m.Update(PickerRemoveMsg{Key: "nobody"})
	m = updated.(PickerModel)
	if len(m.items) != 1 {
		t.Fatalf("removing an unknown key changed the list: %+v", m.items)
	}

	// The index must be consistent after a removal, or re-adding the row
	// would be dropped as a duplicate.
	updated, _ = m.Update(PickerAddMsg{Items: []PickerItem{{Name: "Gentle Forest", DedupKey: "wendyos-gentle-forest"}}})
	m = updated.(PickerModel)
	if len(m.items) != 2 {
		t.Fatalf("re-adding a removed row was dropped: %+v", m.items)
	}
}
