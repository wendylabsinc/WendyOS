package backend

import "testing"

func TestEmmaGalleryMetadataUsesRecordedTemperatureAndEventIdentity(t *testing.T) {
	payload := []byte(`{"type":"event","attributes":{"max_celsius":82.4,"notification_id":"12345678-1234-4234-8234-123456789012"}}
{"type":"event","attributes":{"max_celsius":75}}
{"type":"event","attributes":{"max_celsius":-999}}
{"type":"measurement","attributes":{"notification_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}}`)
	metadata := galleryMetadata("", payload, true)
	if metadata.MaximumCelsius == nil || *metadata.MaximumCelsius != 82.4 {
		t.Fatalf("maximum = %v", metadata.MaximumCelsius)
	}
	if len(metadata.NotificationIDs) != 1 || metadata.NotificationIDs[0] != "12345678-1234-4234-8234-123456789012" {
		t.Fatalf("notification IDs = %v", metadata.NotificationIDs)
	}
	if galleryMetadata("", []byte(`{"attributes":{"raw_count":30000}}`), true).MaximumCelsius != nil {
		t.Fatal("raw counts inferred Celsius")
	}
}
