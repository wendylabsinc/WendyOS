package data

// EpisodeFileMedia projects existing manifest file metadata into list responses.
// MIME types and paths are copied verbatim; classification belongs to the client.
type EpisodeFileMedia struct {
	Path      string `json:"path"`
	MediaType string `json:"media_type"`
}

func episodeMediaFiles(files []File) []EpisodeFileMedia {
	out := make([]EpisodeFileMedia, 0, len(files))
	for _, f := range files {
		out = append(out, EpisodeFileMedia{Path: f.Path, MediaType: f.MediaType})
	}
	return out
}
