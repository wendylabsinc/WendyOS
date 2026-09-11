// Command episode-playable turns a downloaded episode's camera capture into
// playable MP4 files, one per camera source, with per-frame presentation
// timestamps read from the episode's own cameras/<source>/index.jsonl.
//
// It exists because episode camera capture keeps a bare H.264 Annex-B
// elementary stream, which carries no container and therefore no timing, so
// players invent a frame rate and the clip runs at the wrong speed. Supplying
// a fixed rate instead is wrong by construction: the capture rate varies with
// device load. See the episodeexport package for the full reasoning.
//
// The episode directory is only read. Output goes to a separate directory,
// because the elementary stream and its index are checksummed archival truth
// and index.jsonl addresses frames by byte offset within the stream.
//
// This is the natural body of a future "wendy data export" verb; it is a
// standalone command for now so it can ship without touching the CLI's command
// surface.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wendylabsinc/wendy/go/internal/episodeexport"
)

func main() {
	out := flag.String("o", "", "directory to write playable files into (default \"<episode>-playable\" beside the episode)")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), `usage: episode-playable [-o <output-dir>] <episode-dir>

Writes one playable MP4 per camera source found under <episode-dir>/cameras,
timed from each frame's canonical_episode_nanos in that source's index.jsonl.
The episode directory is never modified.
`)
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	episode := filepath.Clean(flag.Arg(0))
	dest := *out
	if dest == "" {
		dest = episode + "-playable"
	}

	// Episodes sealed since the agent learned to remux at seal time already
	// carry cameras/<source>/playable.mp4, listed in their manifest. When
	// every camera source has one there is nothing to convert; reconverting
	// would only duplicate bytes the manifest already vouches for.
	existing, _ := filepath.Glob(filepath.Join(episode, "cameras", "*", episodeexport.PlayableFileName))
	indexes, _ := filepath.Glob(filepath.Join(episode, "cameras", "*", "index.jsonl"))
	if len(existing) > 0 && len(existing) >= len(indexes) {
		fmt.Println("episode already carries playable files written at seal time; nothing to convert:")
		for _, p := range existing {
			fmt.Printf("  %s\n", p)
		}
		return
	}

	results, errs := episodeexport.Convert(episode, dest)
	for _, r := range results {
		if r.Output == "" {
			continue
		}
		fmt.Printf("%s\n", r.Output)
		fmt.Printf("  index lines      %d\n", r.IndexLines)
		fmt.Printf("  frames written   %d\n", r.Frames)
		fmt.Printf("  segments read    %d\n", r.Segments)
		if r.Skipped > 0 {
			fmt.Printf("  frames skipped   %d (bytes missing or truncated)\n", r.Skipped)
		}
		if r.Unparsed > 0 {
			fmt.Printf("  index lines unusable %d (expected for an interrupted episode)\n", r.Unparsed)
		}
		fmt.Printf("  index span       %s\n", r.Span)
		fmt.Printf("  interval min/mean/max  %s / %s / %s\n", r.MinInterval, r.MeanInterval, r.MaxInterval)
		if r.SkippedReason != "" {
			fmt.Fprintf(os.Stderr, "episode-playable: warning: camera %s skipped %d frame(s); first cause: %s\n", r.Source, r.Skipped, r.SkippedReason)
		}
		if r.SyncSamples == 0 {
			fmt.Fprintf(os.Stderr, "episode-playable: warning: camera %s clip carries no random-access frame, so players cannot seek in it and many will not open it at all\n", r.Source)
		}
		if r.ParameterSetChanges > 0 {
			fmt.Fprintf(os.Stderr, "episode-playable: warning: camera %s changes its SPS/PPS mid-stream (%d changed unit(s), a producer restart); the clip carries only the first decoder configuration, so frames after the change will misdecode\n", r.Source, r.ParameterSetChanges)
		}
		if r.BFrames {
			fmt.Fprintf(os.Stderr, "episode-playable: warning: camera %s contains B slices, so its presentation order differs from the coded order index.jsonl records; the timing written for it is approximate\n", r.Source)
		} else if r.UndecodedSliceHeaders > 0 {
			fmt.Fprintf(os.Stderr, "episode-playable: warning: camera %s has %d slice header(s) this tool could not parse, so whether the stream carries B slices is unknown and its timing may be approximate\n", r.Source, r.UndecodedSliceHeaders)
		}
	}
	// Convert returns an empty result list only alongside a non-empty error
	// list, so failing here covers the "nothing was converted" case too.
	if len(errs) > 0 {
		for _, err := range errs {
			fmt.Fprintf(os.Stderr, "episode-playable: %v\n", err)
		}
		os.Exit(1)
	}
}
