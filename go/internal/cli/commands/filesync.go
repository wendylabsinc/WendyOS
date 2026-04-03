package commands

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/dustin/go-humanize"
	"golang.org/x/term"

	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// fileSyncEntry pairs an absolute local path with its effective remote destination.
//   - If localRoot is a regular file, remotePath is the full agent-relative path.
//   - If localRoot is a directory, remotePath is the agent-relative prefix (may be empty).
type fileSyncEntry struct {
	localRoot  string // absolute path on the developer's machine (file or dir)
	remotePath string // agent-relative path (full path for file; prefix for dir)
}

// buildLocalManifest walks root (a directory) and returns a FileSyncEntry for
// every regular file found: path relative to root, size, SHA256 hex, and Unix
// file mode as uint32. Symlinks and non-regular files are skipped.
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
			Sha256: hex.EncodeToString(h.Sum(nil)),
			Mode:   uint32(info.Mode()),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// diffManifests returns the agent-relative paths of files that are missing from
// the agent's manifest or whose SHA256 differs from the local manifest.
// Agent-only files (stale files) are not included; the agent handles deletions.
func diffManifests(local, remote []*agentpb.FileSyncEntry) []string {
	remoteByPath := make(map[string]string, len(remote)) // path → sha256
	for _, e := range remote {
		remoteByPath[e.Path] = e.Sha256
	}

	var toTransfer []string
	for _, e := range local {
		if remoteHash, ok := remoteByPath[e.Path]; !ok || remoteHash != e.Sha256 {
			toTransfer = append(toTransfer, e.Path)
		}
	}
	return toTransfer
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
	agentManifest := agentManifestMsg.Manifest.GetFiles()

	// Compute diff.
	toTransfer := diffManifests(localManifest, agentManifest)

	if len(toTransfer) == 0 {
		// Nothing to transfer. Close stream and wait for complete.
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
	localByPath := make(map[string]*agentpb.FileSyncEntry, len(localManifest))
	for _, e := range localManifest {
		localByPath[e.Path] = e
	}
	var totalBytes int64
	for _, path := range toTransfer {
		if e, ok := localByPath[path]; ok {
			totalBytes += e.Size
		}
	}

	isTTY := term.IsTerminal(int(os.Stdout.Fd()))
	fileCount := len(toTransfer)
	fileIdx := 0
	var sentBytes int64

	cliLogln("Syncing files...")

	// Transfer each file.
	const chunkSize = 256 * 1024
	for _, agentPath := range toTransfer {
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
		fileDisplayName := agentPath

		for {
			n, readErr := f.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				h.Write(chunk)
				if err := stream.Send(&agentpb.FileSyncRequest{
					RequestType: &agentpb.FileSyncRequest_Chunk{
						Chunk: &agentpb.FileSyncChunk{
							Path: agentPath,
							Data: chunk,
						},
					},
				}); err != nil {
					f.Close()
					return fmt.Errorf("sending chunk for %s: %w", agentPath, err)
				}
				fileSent += int64(n)
				sentBytes += int64(n)

				printFileSyncProgress(isTTY, fileDisplayName, fileSent, entry.Size,
					sentBytes, totalBytes, fileIdx+1, fileCount)
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				f.Close()
				return fmt.Errorf("reading %s: %w", localPath, readErr)
			}
		}
		f.Close()

		// Verify the file wasn't mutated during streaming.
		streamingHash := hex.EncodeToString(h.Sum(nil))
		if streamingHash != entry.Sha256 {
			return fmt.Errorf("file %q changed during transfer (manifest sha256 %s, streamed %s)",
				agentPath, entry.Sha256, streamingHash)
		}

		// Send FileSyncCommit.
		if err := stream.Send(&agentpb.FileSyncRequest{
			RequestType: &agentpb.FileSyncRequest_Commit{
				Commit: &agentpb.FileSyncCommit{
					Path:   agentPath,
					Sha256: entry.Sha256,
					Size:   entry.Size,
				},
			},
		}); err != nil {
			return fmt.Errorf("sending commit for %s: %w", agentPath, err)
		}

		// Wait for FileSyncAck.
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
		if isTTY {
			fmt.Print("\n")
		}
	}

	// Signal EOF to the agent (triggers stale-file pruning).
	if err := stream.CloseSend(); err != nil {
		return fmt.Errorf("closing stream: %w", err)
	}

	// Wait for FileSyncComplete.
	resp, err = stream.Recv()
	if err != nil && err != io.EOF {
		return fmt.Errorf("receiving complete: %w", err)
	}
	if err == nil {
		if _, ok := resp.ResponseType.(*agentpb.FileSyncResponse_Complete); !ok {
			return fmt.Errorf("expected FileSyncComplete, got %T", resp.ResponseType)
		}
	}

	cliLogln("Total: %s in %d file(s)", humanize.Bytes(uint64(totalBytes)), fileCount)
	return nil
}

// buildCombinedManifest assembles the local manifest from all fileSyncEntry values
// and returns:
//   - the combined []*agentpb.FileSyncEntry for the FileSyncStart message
//   - a map from agent-relative path → absolute local path (for chunk transfer)
func buildCombinedManifest(entries []fileSyncEntry) ([]*agentpb.FileSyncEntry, map[string]string, error) {
	var manifest []*agentpb.FileSyncEntry
	localFiles := make(map[string]string)

	for _, e := range entries {
		info, err := os.Stat(e.localRoot)
		if err != nil {
			return nil, nil, fmt.Errorf("stat %s: %w", e.localRoot, err)
		}

		if !info.IsDir() {
			// Single file: compute the entry directly.
			agentPath := e.remotePath
			h := sha256.New()
			f, err := os.Open(e.localRoot)
			if err != nil {
				return nil, nil, fmt.Errorf("open %s: %w", e.localRoot, err)
			}
			if _, err := io.Copy(h, f); err != nil {
				f.Close()
				return nil, nil, fmt.Errorf("hashing %s: %w", e.localRoot, err)
			}
			f.Close()

			manifest = append(manifest, &agentpb.FileSyncEntry{
				Path:   agentPath,
				Size:   info.Size(),
				Sha256: hex.EncodeToString(h.Sum(nil)),
				Mode:   uint32(info.Mode()),
			})
			localFiles[agentPath] = e.localRoot
		} else {
			// Directory: walk and prefix paths.
			subEntries, err := buildLocalManifest(e.localRoot)
			if err != nil {
				return nil, nil, fmt.Errorf("building manifest for %s: %w", e.localRoot, err)
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
				manifest = append(manifest, se)
				localFiles[agentPath] = filepath.Join(e.localRoot, relPath)
			}
		}
	}

	return manifest, localFiles, nil
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
