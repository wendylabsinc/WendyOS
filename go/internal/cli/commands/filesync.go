package commands

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dustin/go-humanize"
	"golang.org/x/term"

	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// fileSyncEntry pairs an absolute local path with its effective remote destination.
//   - If localPath is a regular file, remotePath is the full agent-relative path.
//   - If localPath is a directory, remotePath is the agent-relative prefix (may be empty).
type fileSyncEntry struct {
	localPath  string // absolute path on the developer's machine (file or dir)
	remotePath string // agent-relative path (full path for file; prefix for dir)
}

// buildLocalManifest walks root (a directory) and returns a FileSyncEntry for
// every regular file found: path relative to root, size, SHA256 bytes, and
// Unix permission bits as uint32. Symlinks and non-regular files are skipped.
func buildLocalManifest(root string) ([]*agentpb.FileSyncEntry, error) {
	var entries []*agentpb.FileSyncEntry

	err := fs.WalkDir(os.DirFS(root), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("stat %s: %w", path, err)
		}

		absPath := filepath.Join(root, path)
		h := sha256.New()
		f, err := os.Open(absPath)
		if err != nil {
			return fmt.Errorf("open %s: %w", absPath, err)
		}
		defer f.Close()

		if _, err := io.Copy(h, f); err != nil {
			return fmt.Errorf("hashing %s: %w", absPath, err)
		}

		entries = append(entries, &agentpb.FileSyncEntry{
			Path:   path,
			Size:   info.Size(),
			Sha256: append([]byte(nil), h.Sum(nil)...),
			Mode:   uint32(info.Mode().Perm()),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

type modeOnlyChange struct {
	path    string
	oldMode uint32
	newMode uint32
	entry   *agentpb.FileSyncEntry
}

type manifestDiff struct {
	contentTransfers []string
	modeOnly         []modeOnlyChange
	staleRemote      []string
}

// diffManifests compares the full file identity and classifies files into
// content transfers, mode-only changes, and stale remote files.
func diffManifests(local, remote *agentpb.FileSyncManifest) manifestDiff {
	remoteByPath := make(map[string]*agentpb.FileSyncEntry, len(remote.GetFiles()))
	for _, e := range remote.GetFiles() {
		remoteByPath[e.Path] = e
	}

	localPaths := make(map[string]struct{}, len(local.GetFiles()))
	var diff manifestDiff
	for _, e := range local.GetFiles() {
		localPaths[e.Path] = struct{}{}
		remoteEntry, ok := remoteByPath[e.Path]
		if !ok || remoteEntry == nil {
			diff.contentTransfers = append(diff.contentTransfers, e.Path)
			continue
		}

		sameContent := remoteEntry.Size == e.Size && bytes.Equal(remoteEntry.Sha256, e.Sha256)
		sameMode := remoteEntry.Mode == e.Mode
		switch {
		case !sameContent:
			diff.contentTransfers = append(diff.contentTransfers, e.Path)
		case !sameMode:
			diff.modeOnly = append(diff.modeOnly, modeOnlyChange{
				path:    e.Path,
				oldMode: remoteEntry.Mode,
				newMode: e.Mode,
				entry:   e,
			})
		}
	}

	for _, e := range remote.GetFiles() {
		if _, ok := localPaths[e.Path]; !ok {
			diff.staleRemote = append(diff.staleRemote, e.Path)
		}
	}

	sort.Strings(diff.contentTransfers)
	sort.Slice(diff.modeOnly, func(i, j int) bool {
		return diff.modeOnly[i].path < diff.modeOnly[j].path
	})
	sort.Strings(diff.staleRemote)
	return diff
}

// syncFiles drives a complete SyncFiles session:
//  1. Builds the combined local manifest from all entries.
//  2. Exchanges it with the agent (agent replies with its own manifest).
//  3. Diffs the two manifests.
//  4. Transfers only what changed, streaming in 256 KiB chunks.
//  5. Waits for FileSyncComplete.
//
// Progress is printed to stdout when there is something to transfer.
func syncFiles(
	ctx context.Context,
	conn *grpcclient.AgentConnection,
	appID string,
	entries []fileSyncEntry,
) error {
	// Build the combined local manifest and a map from agent-relative path → local abs path.
	localManifest, localFiles, err := buildCombinedManifest(entries)
	if err != nil {
		return fmt.Errorf("building local manifest: %w", err)
	}

	// Open bidi stream.
	stream, err := conn.FileSyncService.SyncFiles(ctx)
	if err != nil {
		return fmt.Errorf("opening SyncFiles stream: %w", err)
	}

	// Send FileSyncStart with the local manifest.
	if err := stream.Send(&agentpb.FileSyncRequest{
		RequestType: &agentpb.FileSyncRequest_Start{
			Start: &agentpb.FileSyncStart{
				AppId:    appID,
				Manifest: localManifest,
			},
		},
	}); err != nil {
		return fmt.Errorf("sending FileSyncStart: %w", err)
	}

	// Receive FileSyncManifest from agent.
	resp, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("receiving agent manifest: %w", err)
	}
	agentManifestMsg, ok := resp.ResponseType.(*agentpb.FileSyncResponse_Manifest)
	if !ok {
		return fmt.Errorf("expected FileSyncManifest, got %T", resp.ResponseType)
	}

	// Compute diff.
	diff := diffManifests(localManifest, agentManifestMsg.Manifest)

	if len(diff.contentTransfers) == 0 && len(diff.modeOnly) == 0 && len(diff.staleRemote) == 0 {
		if err := stream.CloseSend(); err != nil {
			return fmt.Errorf("closing stream: %w", err)
		}
		resp, err := stream.Recv()
		if err != nil && err != io.EOF {
			return fmt.Errorf("receiving complete: %w", err)
		}
		if err == nil {
			if _, ok := resp.ResponseType.(*agentpb.FileSyncResponse_Complete); !ok {
				return fmt.Errorf("expected FileSyncComplete, got %T", resp.ResponseType)
			}
		}
		cliLogln("Files up to date.")
		return nil
	}

	// Compute total bytes to transfer for progress display.
	localByPath := make(map[string]*agentpb.FileSyncEntry, len(localManifest.GetFiles()))
	for _, e := range localManifest.GetFiles() {
		localByPath[e.Path] = e
	}
	var totalBytes int64
	for _, path := range diff.contentTransfers {
		if e, ok := localByPath[path]; ok {
			totalBytes += e.Size
		}
	}

	isTTY := term.IsTerminal(int(os.Stdout.Fd()))
	fileCount := len(diff.contentTransfers)
	fileIdx := 0
	var sentBytes int64

	if fileCount > 0 {
		cliLogln("Syncing files...")
	}

	// Transfer each file.
	const chunkSize = 256 * 1024
	for _, agentPath := range diff.contentTransfers {
		localPath, ok := localFiles[agentPath]
		if !ok {
			return fmt.Errorf("no local path for %q", agentPath)
		}

		entry, ok := localByPath[agentPath]
		if !ok {
			return fmt.Errorf("no manifest entry for %q", agentPath)
		}

		f, err := os.Open(localPath)
		if err != nil {
			return fmt.Errorf("opening %s: %w", localPath, err)
		}

		h := sha256.New()
		buf := make([]byte, chunkSize)
		var fileSent int64
		var sequence uint64
		fileDisplayName := agentPath

		sendChunk := func(data []byte) error {
			if _, err := h.Write(data); err != nil {
				return err
			}
			fileSent += int64(len(data))
			sentBytes += int64(len(data))
			checkpoint := append([]byte(nil), h.Sum(nil)...)
			return stream.Send(&agentpb.FileSyncRequest{
				RequestType: &agentpb.FileSyncRequest_Chunk{
					Chunk: &agentpb.FileSyncChunk{
						Path:           agentPath,
						Data:           append([]byte(nil), data...),
						Sequence:       sequence,
						CumulativeSize: fileSent,
						Sha256:         checkpoint,
					},
				},
			})
		}

		if entry.Size == 0 {
			if err := sendChunk(nil); err != nil {
				f.Close()
				return fmt.Errorf("sending empty chunk for %s: %w", agentPath, err)
			}
		} else {
			for {
				n, readErr := f.Read(buf)
				if n > 0 {
					if err := sendChunk(buf[:n]); err != nil {
						f.Close()
						return fmt.Errorf("sending chunk for %s: %w", agentPath, err)
					}
					printFileSyncProgress(isTTY, fileDisplayName, fileSent, entry.Size,
						sentBytes, totalBytes, fileIdx+1, fileCount)
					sequence++
				}
				if readErr == io.EOF {
					break
				}
				if readErr != nil {
					f.Close()
					return fmt.Errorf("reading %s: %w", localPath, readErr)
				}
			}
		}
		f.Close()

		streamingHash := h.Sum(nil)
		if fileSent != entry.Size {
			return fmt.Errorf("file %q changed during transfer (manifest size %d, streamed %d)",
				agentPath, entry.Size, fileSent)
		}
		if !bytes.Equal(streamingHash, entry.Sha256) {
			return fmt.Errorf("file %q changed during transfer", agentPath)
		}

		if err := stream.Send(&agentpb.FileSyncRequest{
			RequestType: &agentpb.FileSyncRequest_Commit{
				Commit: &agentpb.FileSyncCommit{
					Path:   agentPath,
					Sha256: append([]byte(nil), entry.Sha256...),
					Size:   entry.Size,
				},
			},
		}); err != nil {
			return fmt.Errorf("sending commit for %s: %w", agentPath, err)
		}

		resp, err := stream.Recv()
		if err != nil {
			return fmt.Errorf("receiving ack for %s: %w", agentPath, err)
		}
		ack, ok := resp.ResponseType.(*agentpb.FileSyncResponse_Ack)
		if !ok {
			return fmt.Errorf("expected FileSyncAck for %s, got %T", agentPath, resp.ResponseType)
		}
		if ack.Ack.Path != agentPath {
			return fmt.Errorf("ack path mismatch: expected %q, got %q", agentPath, ack.Ack.Path)
		}

		fileIdx++
		if isTTY && entry.Size > 0 {
			fmt.Print("\n")
		}
	}

	for _, change := range diff.modeOnly {
		cliLogln("mode changed: %s %04o -> %04o", change.path, change.oldMode, change.newMode)
		if err := stream.Send(&agentpb.FileSyncRequest{
			RequestType: &agentpb.FileSyncRequest_SetMode{
				SetMode: &agentpb.FileSyncSetMode{
					Path:   change.path,
					Mode:   change.entry.Mode,
					Size:   change.entry.Size,
					Sha256: append([]byte(nil), change.entry.Sha256...),
				},
			},
		}); err != nil {
			return fmt.Errorf("sending mode update for %s: %w", change.path, err)
		}

		resp, err := stream.Recv()
		if err != nil {
			return fmt.Errorf("receiving ack for %s: %w", change.path, err)
		}
		ack, ok := resp.ResponseType.(*agentpb.FileSyncResponse_Ack)
		if !ok {
			return fmt.Errorf("expected FileSyncAck for %s, got %T", change.path, resp.ResponseType)
		}
		if ack.Ack.Path != change.path {
			return fmt.Errorf("ack path mismatch: expected %q, got %q", change.path, ack.Ack.Path)
		}
	}

	for _, path := range diff.staleRemote {
		cliLogln("deleted: %s", path)
	}

	if err := stream.CloseSend(); err != nil {
		return fmt.Errorf("closing stream: %w", err)
	}

	resp, err = stream.Recv()
	if err != nil && err != io.EOF {
		return fmt.Errorf("receiving complete: %w", err)
	}
	if err == nil {
		if _, ok := resp.ResponseType.(*agentpb.FileSyncResponse_Complete); !ok {
			return fmt.Errorf("expected FileSyncComplete, got %T", resp.ResponseType)
		}
	}

	if fileCount > 0 {
		cliLogln("Total: %s in %d file(s)", humanize.Bytes(uint64(totalBytes)), fileCount)
	}
	return nil
}

// buildCombinedManifest assembles the local manifest from all fileSyncEntry values
// and returns:
//   - the combined FileSyncManifest for the FileSyncStart message
//   - a map from agent-relative path → absolute local path (for chunk transfer)
func buildCombinedManifest(entries []fileSyncEntry) (*agentpb.FileSyncManifest, map[string]string, error) {
	var files []*agentpb.FileSyncEntry
	localFiles := make(map[string]string)

	for _, e := range entries {
		info, err := os.Stat(e.localPath)
		if err != nil {
			return nil, nil, fmt.Errorf("stat %s: %w", e.localPath, err)
		}

		if !info.IsDir() {
			// Single file: compute the entry directly.
			agentPath := e.remotePath
			h := sha256.New()
			f, err := os.Open(e.localPath)
			if err != nil {
				return nil, nil, fmt.Errorf("open %s: %w", e.localPath, err)
			}
			if _, err := io.Copy(h, f); err != nil {
				f.Close()
				return nil, nil, fmt.Errorf("hashing %s: %w", e.localPath, err)
			}
			f.Close()

			files = append(files, &agentpb.FileSyncEntry{
				Path:   agentPath,
				Size:   info.Size(),
				Sha256: append([]byte(nil), h.Sum(nil)...),
				Mode:   uint32(info.Mode().Perm()),
			})
			localFiles[agentPath] = e.localPath
		} else {
			// Directory: walk and prefix paths.
			subEntries, err := buildLocalManifest(e.localPath)
			if err != nil {
				return nil, nil, fmt.Errorf("building manifest for %s: %w", e.localPath, err)
			}
			for _, se := range subEntries {
				relPath := se.Path
				var agentPath string
				if e.remotePath != "" {
					agentPath = e.remotePath + "/" + relPath
				} else {
					agentPath = relPath
				}
				se.Path = agentPath
				files = append(files, se)
				localFiles[agentPath] = filepath.Join(e.localPath, relPath)
			}
		}
	}

	return &agentpb.FileSyncManifest{Files: files}, localFiles, nil
}

// printFileSyncProgress prints a single-line progress update for the current file.
// On a TTY it overwrites the current line; otherwise it prints a new line.
func printFileSyncProgress(isTTY bool, name string, fileSent, fileTotal, totalSent, grandTotal int64, fileIdx, fileCount int) {
	pct := 0.0
	if fileTotal > 0 {
		pct = float64(fileSent) / float64(fileTotal) * 100
	}

	// Truncate long names.
	displayName := name
	const maxNameLen = 32
	if len(displayName) > maxNameLen {
		displayName = "..." + displayName[len(displayName)-maxNameLen+3:]
	}

	line := fmt.Sprintf("  %-36s %8s / %-8s %5.1f%%   [%d/%d]",
		displayName,
		humanize.Bytes(uint64(fileSent)),
		humanize.Bytes(uint64(fileTotal)),
		pct,
		fileIdx, fileCount,
	)

	if isTTY {
		fmt.Printf("\r\033[2K%s", line)
	} else {
		fmt.Println(line)
	}
}

// effectiveRemotePath returns the effective destination path on the device for
// a FileSyncEntry from AppConfig. When To is empty it defaults to Path with any
// leading "./" stripped.
func effectiveRemotePath(path, to string) string {
	if to != "" {
		return to
	}
	return strings.TrimPrefix(path, "./")
}
