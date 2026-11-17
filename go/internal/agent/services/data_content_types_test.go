package services

import (
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"testing"
)

func TestEpisodeContentTypesPresence(t *testing.T) {
	for _, types := range [][]string{nil, {"video", "audio"}} {
		row := infoEpisode(data.EpisodeInfo{ContentTypes: types})
		if !row.ContentTypesKnown || len(row.ContentTypes) != len(types) {
			t.Fatalf("lost summary: %v", row)
		}
	}
	if inProgressEpisode(data.InProgressEpisode{}).ContentTypesKnown {
		t.Fatal("unfinished episode cannot claim an exhaustive summary")
	}
}
