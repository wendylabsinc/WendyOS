package data

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"

	"github.com/wendylabsinc/wendy/go/internal/episodeexport"
)

// muxPlayableClips writes cameras/<source>/playable.mp4 for every camera
// source of a finished episode. It runs while the episode is being sealed,
// before sealFiles walks the directory, so the derived clip gets a size and
// SHA-256 entry in the manifest and is uploaded and verified exactly like the
// capture it derives from. The point is browser playback straight from the
// bucket: browsers cannot play the raw H.264 elementary stream, and the remux
// gives every frame the presentation time cameras/<source>/index.jsonl
// recorded for it.
//
// It never fails or blocks the seal. The remux is a copy, not a transcode, so
// its cost is one more pass over the camera bytes beside the checksum pass the
// seal already makes, and the clip it leaves is roughly the size of the raw
// stream it derives from; that size is counted against the episode by both
// the manifest total and the disk-usage walk enforceQuota performs. A source
// whose stream cannot become an honestly timed, seekable clip (B slices,
// slice headers the muxer cannot parse, no random-access frame, or an
// outright mux failure) seals without its playable.mp4, and the returned
// notes, published as the manifest's playable_notes, name why. A clip that
// was written but had to omit frames whose bytes were missing gets a note
// too, so the manifest never presents a partial clip as a complete one.
//
// The raw capture is never touched: index.jsonl keeps addressing frames by
// byte offset into the raw segments, so the correlation join between a frame
// and the model input recorded against it is unaffected.
func muxPlayableClips(dir string) []string {
	indexes, err := filepath.Glob(filepath.Join(dir, "cameras", "*", "index.jsonl"))
	if err != nil || len(indexes) == 0 {
		return nil
	}
	sort.Strings(indexes)
	var notes []string
	for _, index := range indexes {
		sourceDir := filepath.Dir(index)
		rel := path.Join("cameras", filepath.Base(sourceDir), episodeexport.PlayableFileName)
		result, err := convertPlayableClip(dir, sourceDir)
		if reason := playableSkipReason(result, err); reason != "" {
			// ConvertSourceInPlace only leaves a file behind on success, but a
			// clip refused by policy (B slices and the like) was written before
			// the result could be judged, so it is removed rather than shipped
			// with timing nobody can vouch for.
			_ = os.Remove(filepath.Join(sourceDir, episodeexport.PlayableFileName))
			notes = append(notes, fmt.Sprintf("%s not written: %s", rel, reason))
			continue
		}
		if result.Skipped > 0 {
			note := fmt.Sprintf("%s omits %d of %d indexed frame(s)", rel, result.Skipped, result.IndexLines)
			if result.SkippedReason != "" {
				note += ": " + result.SkippedReason
			}
			notes = append(notes, note)
		}
		if result.NominalHold > 0 {
			notes = append(notes, fmt.Sprintf(
				"%s holds a single frame whose display duration no index entry records; it was given a nominal %s",
				rel, result.NominalHold))
		}
	}
	return notes
}

// convertClip is the remux entry point. It is a variable only so a test can
// drive convertPlayableClip's recover without having to find a fixture that
// makes the muxer panic for real.
var convertClip = episodeexport.ConvertSourceInPlace

// convertPlayableClip is ConvertSourceInPlace with a bounded recover.
//
// The remux parses attacker-shaped-in-principle data (an index and segment
// files that a crashed or corrupted capture may have left in any state) while
// running inside the seal. A panic there would abort the seal and lose the
// episode itself, which is a far worse outcome than losing a derived clip that
// can be rebuilt from the raw capture at any time. Turning the panic into an
// ordinary mux error keeps the existing refusal path: the clip is not written
// and the manifest's playable_notes name the reason.
func convertPlayableClip(dir, sourceDir string) (result episodeexport.ClipResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("remuxing panicked: %v", r)
		}
	}()
	return convertClip(dir, sourceDir)
}

// playableSkipReason decides whether a mux result is honest enough to publish
// in the sealed episode, returning the reason to refuse it or "" to keep it.
// The gates mirror the warnings the episode-playable command prints, but at
// seal time they are hard: a clip whose timing or seekability the muxer
// cannot vouch for is not listed in a manifest that vouches for everything it
// lists.
func playableSkipReason(r episodeexport.ClipResult, err error) string {
	switch {
	case err != nil:
		return err.Error()
	case r.Frames == 0:
		return "no frame payload could be muxed"
	case r.BFrames:
		return "stream carries B slices, so its presentation order differs from the coded order index.jsonl records and the clip's timing would be wrong"
	case r.UndecodedSliceHeaders > 0:
		return fmt.Sprintf("%d slice header(s) could not be parsed, so whether the stream carries B slices is unknown and the clip's timing cannot be vouched for", r.UndecodedSliceHeaders)
	case r.ParameterSetChanges > 0:
		return fmt.Sprintf("the stream's parameter sets change mid-episode (%d changed SPS/PPS unit(s), a producer restart), and the clip's single decoder configuration would misdecode every frame after the change", r.ParameterSetChanges)
	case r.TimestampInversions > 0:
		return fmt.Sprintf("%d index entry/entries record a canonical timestamp earlier than the entry before them, so the recorded timing disagrees with the coded order the clip must preserve and its timing cannot be vouched for", r.TimestampInversions)
	case r.SyncSamples == 0:
		return "clip would carry no random-access frame, so players cannot seek in it and many will not open it"
	}
	return ""
}

// isDerivedPlayable reports whether a manifest-relative path names a
// seal-time derived camera remux, which sealFiles marks with FileRoleDerived.
func isDerivedPlayable(rel string) bool {
	dir, base := path.Split(rel)
	return base == episodeexport.PlayableFileName && path.Dir(path.Clean(dir)) == "cameras"
}
