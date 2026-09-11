package commands

// Native app-layer builds for Stagefile projects: when an iteration changes
// only `copy: from: local` files, the CLI rebuilds those COPY layers itself —
// deterministically, without Docker — and splices them into the persistent
// OCI layout directory the chunk-diff deploy already maintains. Everything
// here mirrors what BuildKit's COPY would produce semantically (same files,
// same modes, root:root ownership) but NOT byte-identically: native layers
// zero the mtimes, so the same input files always produce the same diff ID on
// any machine.

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/gzip"
	"github.com/wendylabsinc/wendy/go/internal/stagefile"
	"github.com/wendylabsinc/wendy/go/internal/stagefile/cachekey"
	"github.com/wendylabsinc/wendy/go/internal/stagefile/ir"
	"github.com/wendylabsinc/wendy/go/internal/stagefile/lock"
	"github.com/wendylabsinc/wendy/go/internal/stagefile/spec"
)

// nativeDepsHash covers every build input EXCEPT the final stage's local copy
// files: the generated Dockerfile bytes, the Stagefile lockfile, every
// install-referenced file (pip requirements, package.json + lockfile), local
// copies feeding NON-final stages, the platform, and the sorted build args.
// Any change here means the deps layers may differ → the buildx path must run.
func nativeDepsHash(cwd, dockerfile, platform string, buildArgs map[string]string, sf *spec.File) (string, error) {
	h := sha256.New()
	io.WriteString(h, "wendy-native-deps-v1\n")
	io.WriteString(h, "platform="+platform+"\n")

	keys := make([]string, 0, len(buildArgs))
	for k := range buildArgs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		io.WriteString(h, "arg "+k+"="+buildArgs[k]+"\n")
	}

	dfPath, err := confinedDockerfilePath(cwd, dockerfile)
	if err != nil {
		return "", err
	}
	dfData, err := os.ReadFile(dfPath)
	if err != nil {
		return "", fmt.Errorf("reading dockerfile for deps hash: %w", err)
	}
	fmt.Fprintf(h, "dockerfile %d\n", len(dfData))
	h.Write(dfData)

	// The lockfile pins base-image digests; absent is a valid state (hashed as
	// absent — creating it later changes the hash, correctly). Each Stagefile
	// variant owns its own lockfile, so hash the one belonging to the source
	// this dockerfile was compiled from, not whichever happens to be canonical.
	if source, ok := stagefileSourceForGenerated(dockerfile); ok {
		if lockData, err := os.ReadFile(filepath.Join(cwd, stagefile.LockName(source))); err == nil {
			fmt.Fprintf(h, "lock %d\n", len(lockData))
			h.Write(lockData)
		}
	}

	for _, p := range nativeDepsPaths(sf) {
		if err := hashContextPath(h, cwd, p); err != nil {
			return "", err
		}
	}
	semanticKey, err := nativeSemanticDepsKey(cwd, dockerfile, platform, sf)
	if err != nil {
		return "", err
	}
	io.WriteString(h, "semantic="+semanticKey+"\n")
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// nativeSemanticDepsKey keys the semantic graph immediately before the final
// stage's app COPY layers. The existing byte-level inputs above remain during
// rollout as a conservative second guard; this key starts using the same
// dependency boundary the compiler and future cache tiers understand.
func nativeSemanticDepsKey(cwd, dockerfile, platform string, sf *spec.File) (string, error) {
	for _, stage := range sf.Stages {
		if stage.CUDA {
			// Selecting the pinned GPU profile needs the device gpu_arch, which is
			// not available at this local reuse seam. The Dockerfile+lock guards
			// above remain authoritative for these builds.
			return "cuda-deferred", nil
		}
	}
	source, ok := stagefileSourceForGenerated(dockerfile)
	if !ok {
		return "", fmt.Errorf("%s is not generated from a Stagefile", dockerfile)
	}
	l, err := lock.Load(filepath.Join(cwd, stagefile.LockName(source)))
	if err != nil {
		return "", err
	}
	if l == nil {
		return "lock-deferred", nil
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = cwd
	}
	g, err := ir.Lower(sf, ir.Options{Images: l.Images, Downloads: l.Downloads, Platform: platform, CacheScope: abs})
	if err != nil {
		return "", err
	}
	final := g.Stages[len(g.Stages)-1]
	start := 0
	if len(g.Stages) > 1 {
		start = g.Stages[len(g.Stages)-2].Final + 1
	}
	frontier := final.Final
	for i := start; i <= final.Final; i++ {
		n := g.Nodes[i]
		if n.Copy != nil && n.Copy.FromLocal {
			frontier = n.Inputs[0]
			break
		}
	}
	paths := nativeDepsPaths(sf)
	files := make(map[string]string, len(paths))
	for _, path := range paths {
		digest, err := digestContextPath(cwd, path)
		if err != nil {
			return "", err
		}
		files[path] = digest
	}
	return cachekey.Key(g, frontier, cachekey.Inputs{Images: l.Images, Files: files, Platform: platform})
}

func digestContextPath(cwd, rel string) (string, error) {
	h := sha256.New()
	if err := hashContextPath(h, cwd, rel); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// nativeDepsPaths lists the local paths that feed deps layers: every stage's
// install-referenced files plus local copies of NON-final stages. The final
// stage's local copies are the app inputs and are deliberately excluded.
func nativeDepsPaths(sf *spec.File) []string {
	if len(sf.Stages) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var paths []string
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	lastIdx := len(sf.Stages) - 1
	for i := range sf.Stages {
		s := &sf.Stages[i]
		// Shared with the .dockerignore allowlist, deliberately: these two
		// answers must never disagree about which context files an install
		// reads, and they did once (see spec.Install.LocalFiles).
		for _, p := range s.Install.LocalFiles() {
			add(p)
		}
		if i == lastIdx {
			continue
		}
		for _, c := range s.Copy {
			if c.From != "local" {
				continue
			}
			for _, p := range c.Paths {
				add(p)
			}
		}
	}
	sort.Strings(paths)
	return paths
}

// hashContextPath streams the content of a file (or a sorted walk of a
// directory) rooted at rel into h. A missing input is an error — the caller
// treats it as "not natively buildable" and falls back to buildx.
func hashContextPath(h io.Writer, cwd, rel string) error {
	abs := filepath.Join(cwd, filepath.FromSlash(rel))
	fi, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("deps input %q: %w", rel, err)
	}
	hashOne := func(name, file string) error {
		data, err := os.ReadFile(file)
		if err != nil {
			return fmt.Errorf("deps input %q: %w", name, err)
		}
		fmt.Fprintf(h, "file %s %d\n", name, len(data))
		h.Write(data)
		return nil
	}
	if !fi.IsDir() {
		return hashOne(rel, abs)
	}
	var files []string
	err = filepath.WalkDir(abs, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.Type().IsRegular() {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("deps input %q: %w", rel, err)
	}
	sort.Strings(files)
	for _, f := range files {
		relName, err := filepath.Rel(cwd, f)
		if err != nil {
			return err
		}
		if err := hashOne(filepath.ToSlash(relName), f); err != nil {
			return err
		}
	}
	return nil
}

// nativeStateName is the bookkeeping file inside the layout dir recording what
// the native path last built. It sits at the dir root, out of GC's reach
// (GC only touches blobs/sha256).
const nativeStateName = "state.json"

// nativeState records the native path's ground truth: the deps-input hash the
// current layout was built against, the manifest we last wrote (or adopted),
// and our app-layer digests plus exact manifest positions in copy-entry order.
// Positions make the instruction mapping explicit: digest-only replacement is ambiguous when an
// identical layer blob appears more than once in an image. A later run may go
// native only when all of this state still matches reality.
type nativeState struct {
	DepsHash          string   `json:"deps_hash"`
	ManifestDigest    string   `json:"manifest_digest"`
	AppLayerDigests   []string `json:"app_layer_digests"`
	AppLayerPositions []int    `json:"app_layer_positions"`
}

// loadNativeState returns the recorded state, or (nil, false) on any miss or
// malformation — never an error, since the fallback (buildx) is always valid.
func loadNativeState(dir string) (*nativeState, bool) {
	data, err := os.ReadFile(filepath.Join(dir, nativeStateName))
	if err != nil {
		return nil, false
	}
	var s nativeState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, false
	}
	if s.DepsHash == "" || s.ManifestDigest == "" || len(s.AppLayerDigests) == 0 ||
		len(s.AppLayerPositions) != len(s.AppLayerDigests) {
		return nil, false
	}
	for i, pos := range s.AppLayerPositions {
		if pos < 0 || (i > 0 && pos <= s.AppLayerPositions[i-1]) {
			return nil, false
		}
	}
	return &s, true
}

// saveNativeState persists s atomically (temp + rename, 0600).
func saveNativeState(dir string, s nativeState) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".state-*.json")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, nativeStateName))
}

// nativeLayersEnv disables the native fast path when set to "off" (escape
// hatch; the buildx path then always runs).
const nativeLayersEnv = "WENDY_NATIVE_LAYERS"

// nativeBuildEligibility decides whether this build may use the native
// app-layer path. All must hold: the resolved dockerfile is the compiled
// Stagefile output, the Stagefile parses, the final stage does not compile
// (`build:`), and it has at least one `from: local` copy. Local and cross-stage
// copies may be interleaved: the compiler emits every final-stage copy as the
// final ordered filesystem-layer sequence, so adoption can map local entries
// to their exact positions within that sequence.
func nativeBuildEligibility(cwd, dockerfile string) (*spec.File, bool) {
	if os.Getenv(nativeLayersEnv) == "off" {
		return nil, false
	}
	source, ok := stagefileSourceForGenerated(dockerfile)
	if !ok {
		return nil, false
	}
	data, err := os.ReadFile(filepath.Join(cwd, source))
	if err != nil {
		return nil, false
	}
	sf, err := spec.Parse(data)
	if err != nil || len(sf.Stages) == 0 {
		return nil, false
	}
	final := sf.Stages[len(sf.Stages)-1]
	if final.Build != nil {
		return nil, false
	}
	locals := 0
	for _, c := range final.Copy {
		if c.From == "local" {
			locals++
		}
	}
	if locals == 0 {
		return nil, false
	}
	return sf, true
}

// buildOrUpdateOCILayout gives every persistent OCI-layout caller the same
// Stagefile-aware fast path. If only final-stage local copies changed, their
// deterministic layers are rebuilt and content-addressed in-process; Docker,
// the multi-gigabyte local cache exporter, and unchanged dependency layers are
// skipped entirely. A dependency change or any failed safety guard falls back
// to buildx, after which the resulting app layers are adopted for the next
// iteration.
//
// The caller must hold the layout directory lock for the whole call. buildx is
// responsible only for updating the layout; pushing or chunking it remains the
// caller's concern.
func buildOrUpdateOCILayout(cwd, dockerfile, platform string, buildArgs map[string]string, layoutDir string, buildx func() error) (native bool, err error) {
	sf, eligible := nativeBuildEligibility(cwd, dockerfile)
	depsHash := ""
	if eligible {
		if h, hashErr := nativeDepsHash(cwd, dockerfile, platform, buildArgs, sf); hashErr == nil {
			depsHash = h
		} else {
			eligible = false
		}
	}

	if eligible {
		if st, ok := loadNativeState(layoutDir); ok && st.DepsHash == depsHash {
			if done, rebuildErr := tryNativeRebuild(layoutDir, platform, cwd, sf, st); rebuildErr == nil && done {
				return true, nil
			}
		}
	}

	if err := buildx(); err != nil {
		return false, err
	}
	if eligible {
		// Adoption is an optimization. Refusing a layout whose final layers do
		// not exactly match Stagefile's declared local copies leaves the valid
		// buildx image untouched and simply retries buildx next time.
		_, _ = adoptNativeLayers(layoutDir, platform, cwd, sf, depsHash)
	}
	return false, nil
}

// ociManifest captures the standard OCI image-manifest fields the splice
// mutates. Annotations are preserved; anything nonstandard would be dropped,
// which buildx-produced image manifests don't carry.
type ociManifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType,omitempty"`
	Config        ociManifestDesc   `json:"config"`
	Layers        []ociManifestDesc `json:"layers"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

type ociManifestDesc struct {
	MediaType   string            `json:"mediaType,omitempty"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

const nativeLayerMediaType = "application/vnd.oci.image.layer.v1.tar+gzip"

// ociLayoutDirManifest resolves the layout dir's index to the image manifest
// for the target platform and returns its raw bytes plus digest.
func ociLayoutDirManifest(dir, platform string) (manifestData []byte, manifestDigest string, err error) {
	indexJSON, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		return nil, "", fmt.Errorf("read OCI layout index: %w", err)
	}
	var index struct {
		Manifests []ociDescriptor `json:"manifests"`
	}
	if err := json.Unmarshal(indexJSON, &index); err != nil {
		return nil, "", fmt.Errorf("parsing index.json: %w", err)
	}
	getBlob := func(hexDigest string) ([]byte, error) {
		return os.ReadFile(filepath.Join(dir, "blobs", "sha256", hexDigest))
	}
	wantOS, wantArch := parseOCIPlatform(platform)
	manifestData, err = resolveOCIImageManifest(index.Manifests, getBlob, wantOS, wantArch, 0)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(manifestData)
	return manifestData, "sha256:" + hex.EncodeToString(sum[:]), nil
}

// writeLayoutBlob content-addresses data into the layout dir's blob store
// (no-op when already present) and returns its digest.
func writeLayoutBlob(dir string, data []byte) (string, error) {
	sum := sha256.Sum256(data)
	hexDigest := hex.EncodeToString(sum[:])
	blobsDir := filepath.Join(dir, "blobs", "sha256")
	p := filepath.Join(blobsDir, hexDigest)
	if _, err := os.Stat(p); err == nil {
		return "sha256:" + hexDigest, nil
	}
	if err := os.MkdirAll(blobsDir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(blobsDir, ".blob-*")
	if err != nil {
		return "", err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return "sha256:" + hexDigest, nil
}

// spliceNativeLayers rewrites the layout dir's image for the target platform:
// the layers at `positions` become `with` after their current digests are
// verified against `replace`; the config's rootfs.diff_ids follow 1:1, and
// index.json atomically points at the rewritten manifest (any attestation entries are dropped —
// provenance no longer describes the spliced image). Superseded blobs stay on
// disk for the deploy-time GC. Returns the new manifest digest.
func spliceNativeLayers(dir, platform string, positions []int, replace []string, with []*nativeLayer) (string, error) {
	if len(positions) != len(replace) || len(replace) != len(with) {
		return "", fmt.Errorf("splice: %d positions, %d digests, and %d layers given", len(positions), len(replace), len(with))
	}
	manifestData, _, err := ociLayoutDirManifest(dir, platform)
	if err != nil {
		return "", err
	}
	var m ociManifest
	if err := json.Unmarshal(manifestData, &m); err != nil {
		return "", fmt.Errorf("parsing manifest for splice: %w", err)
	}

	cfgHex, err := digestToHex(m.Config.Digest)
	if err != nil {
		return "", fmt.Errorf("splice: invalid config digest %q: %w", m.Config.Digest, err)
	}
	cfgData, err := os.ReadFile(filepath.Join(dir, "blobs", "sha256", cfgHex))
	if err != nil {
		return "", fmt.Errorf("splice: config blob: %w", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(cfgData, &cfg); err != nil {
		return "", fmt.Errorf("splice: parsing config: %w", err)
	}
	rootfs, _ := cfg["rootfs"].(map[string]any)
	diffIDs, _ := rootfs["diff_ids"].([]any)
	if rootfs == nil || len(diffIDs) != len(m.Layers) {
		return "", fmt.Errorf("splice: config diff_ids (%d) do not align with manifest layers (%d)", len(diffIDs), len(m.Layers))
	}

	for i, pos := range positions {
		if pos < 0 || pos >= len(m.Layers) {
			return "", fmt.Errorf("splice: layer position %d is outside manifest with %d layers", pos, len(m.Layers))
		}
		if i > 0 && pos <= positions[i-1] {
			return "", fmt.Errorf("splice: layer positions must be strictly increasing")
		}
		if m.Layers[pos].Digest != replace[i] {
			return "", fmt.Errorf("splice: layer at position %d is %q, want %q", pos, m.Layers[pos].Digest, replace[i])
		}
	}

	for j, pos := range positions {
		nl := with[j]
		if _, err := writeLayoutBlob(dir, nl.Blob); err != nil {
			return "", fmt.Errorf("splice: writing native layer: %w", err)
		}
		m.Layers[pos] = ociManifestDesc{
			MediaType: nativeLayerMediaType,
			Digest:    nl.Digest,
			Size:      int64(len(nl.Blob)),
		}
		diffIDs[pos] = nl.DiffID
	}

	newCfg, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	cfgDigest, err := writeLayoutBlob(dir, newCfg)
	if err != nil {
		return "", fmt.Errorf("splice: writing config: %w", err)
	}
	m.Config.Digest = cfgDigest
	m.Config.Size = int64(len(newCfg))

	newManifest, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	manifestDigest, err := writeLayoutBlob(dir, newManifest)
	if err != nil {
		return "", fmt.Errorf("splice: writing manifest: %w", err)
	}

	wantOS, wantArch := parseOCIPlatform(platform)
	index := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests": []map[string]any{{
			"mediaType": "application/vnd.oci.image.manifest.v1+json",
			"digest":    manifestDigest,
			"size":      len(newManifest),
			"platform":  map[string]any{"os": wantOS, "architecture": wantArch},
		}},
	}
	indexData, err := json.Marshal(index)
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".index-*.json")
	if err != nil {
		return "", err
	}
	if _, err := tmp.Write(indexData); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, "index.json")); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return manifestDigest, nil
}

// nativeAppLayerPositions maps final-stage local copy entries to manifest
// positions. With no final-stage build step, codegen emits the complete copy:
// list as the image's final ordered filesystem-layer sequence. Counting the
// whole list, rather than only local entries, permits local and cross-stage
// copies to be interleaved without guessing which descriptors belong to us.
func nativeAppLayerPositions(manifestLayers int, sf *spec.File) ([]int, bool) {
	if len(sf.Stages) == 0 {
		return nil, false
	}
	final := sf.Stages[len(sf.Stages)-1]
	if final.Build != nil {
		return nil, false
	}
	copies := final.Copy
	if len(copies) == 0 || manifestLayers < len(copies) {
		return nil, false
	}
	base := manifestLayers - len(copies)
	positions := make([]int, 0, len(copies))
	for i, entry := range copies {
		if entry.From == "local" {
			positions = append(positions, base+i)
		}
	}
	return positions, len(positions) > 0
}

// nativeAppCopyEntries returns the final stage's `from: local` copy entries —
// the instructions whose layers the native path owns.
func nativeAppCopyEntries(sf *spec.File) []spec.CopyEntry {
	if len(sf.Stages) == 0 {
		return nil
	}
	var out []spec.CopyEntry
	for _, c := range sf.Stages[len(sf.Stages)-1].Copy {
		if c.From == "local" {
			out = append(out, c)
		}
	}
	return out
}

// configWorkingDir extracts config.WorkingDir from a raw image config blob
// ("" when unset — COPY then resolves against "/").
func configWorkingDir(cfgData []byte) string {
	var cfg struct {
		Config struct {
			WorkingDir string `json:"WorkingDir"`
		} `json:"config"`
	}
	_ = json.Unmarshal(cfgData, &cfg)
	return cfg.Config.WorkingDir
}

// buildNativeAppLayers materializes every final-stage local copy entry.
func buildNativeAppLayers(cwd string, sf *spec.File, workDir string) ([]*nativeLayer, error) {
	entries := nativeAppCopyEntries(sf)
	layers := make([]*nativeLayer, 0, len(entries))
	for _, e := range entries {
		nl, err := buildNativeCopyLayer(cwd, e, workDir)
		if err != nil {
			return nil, err
		}
		layers = append(layers, nl)
	}
	return layers, nil
}

// layoutLayerFileNames decompresses a layout-dir layer blob and returns its
// non-directory tar entry names.
func layoutLayerFileNames(dir string, desc ociManifestDesc) (map[string]bool, error) {
	hexDigest, err := digestToHex(desc.Digest)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "blobs", "sha256", hexDigest))
	if err != nil {
		return nil, err
	}
	r, cleanup, err := layerTarReader(bytes.NewReader(data), desc.MediaType)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	names := map[string]bool{}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		names[strings.TrimPrefix(path.Clean(hdr.Name), "./")] = true
	}
	return names, nil
}

// nativeNonDirNames filters a native layer's entry list down to files/links.
func nativeNonDirNames(l *nativeLayer) map[string]bool {
	names := map[string]bool{}
	for _, n := range l.FileNames {
		if !strings.HasSuffix(n, "/") {
			names[n] = true
		}
	}
	return names
}

// adoptNativeLayers runs after a successful buildx build of an eligible
// project: it rebuilds each final-stage local copy entry natively, sanity
// maps them into the final stage's complete copy-layer tail, checks the mapped
// layers against them (the file-name sets must match), splices, and records
// the verified positions. Returns
// false with no error when the check rejects; the buildx layers then ship
// as-is and the native path stays disabled until the next buildx build.
func adoptNativeLayers(dir, platform, cwd string, sf *spec.File, depsHash string) (bool, error) {
	entries := nativeAppCopyEntries(sf)
	if len(entries) == 0 {
		return false, nil
	}
	manifestData, _, err := ociLayoutDirManifest(dir, platform)
	if err != nil {
		return false, nil
	}
	var m ociManifest
	if err := json.Unmarshal(manifestData, &m); err != nil {
		return false, nil
	}
	positions, ok := nativeAppLayerPositions(len(m.Layers), sf)
	if !ok || len(positions) != len(entries) {
		return false, nil
	}
	cfgHex, err := digestToHex(m.Config.Digest)
	if err != nil {
		return false, nil
	}
	cfgData, err := os.ReadFile(filepath.Join(dir, "blobs", "sha256", cfgHex))
	if err != nil {
		return false, nil
	}

	native, err := buildNativeAppLayers(cwd, sf, configWorkingDir(cfgData))
	if err != nil {
		return false, nil
	}

	replace := make([]string, len(entries))
	for i, nl := range native {
		pos := positions[i]
		got, err := layoutLayerFileNames(dir, m.Layers[pos])
		if err != nil {
			return false, nil
		}
		want := nativeNonDirNames(nl)
		if len(got) != len(want) {
			cliLogln("native layers: buildx layer %d file set differs from copy entry %d; keeping buildx layers", pos, i)
			return false, nil
		}
		for n := range want {
			if !got[n] {
				cliLogln("native layers: buildx layer %d is missing %q; keeping buildx layers", pos, n)
				return false, nil
			}
		}
		replace[i] = m.Layers[pos].Digest
	}

	manifestDigest, err := spliceNativeLayers(dir, platform, positions, replace, native)
	if err != nil {
		return false, err
	}
	digests := make([]string, len(native))
	for i, nl := range native {
		digests[i] = nl.Digest
	}
	if err := saveNativeState(dir, nativeState{
		DepsHash:          depsHash,
		ManifestDigest:    manifestDigest,
		AppLayerDigests:   digests,
		AppLayerPositions: positions,
	}); err != nil {
		return false, err
	}
	return true, nil
}

// tryNativeRebuild is the buildx-free iteration: with deps unchanged and the
// layout still exactly what we last wrote, rebuild the app layers from disk
// and splice them in. Any doubt returns (false, nil) and the buildx path runs.
func tryNativeRebuild(dir, platform, cwd string, sf *spec.File, st *nativeState) (bool, error) {
	_, manifestDigest, err := ociLayoutDirManifest(dir, platform)
	if err != nil || manifestDigest != st.ManifestDigest {
		return false, nil
	}
	entries := nativeAppCopyEntries(sf)
	if len(entries) == 0 || len(entries) != len(st.AppLayerDigests) ||
		len(st.AppLayerPositions) != len(st.AppLayerDigests) {
		return false, nil
	}
	cfgData, _, err := ociLayoutDirConfig(dir, platform)
	if err != nil {
		return false, nil
	}
	native, err := buildNativeAppLayers(cwd, sf, configWorkingDir(cfgData))
	if err != nil {
		return false, nil
	}
	unchanged := len(native) == len(st.AppLayerDigests)
	for i, nl := range native {
		if !unchanged || nl.Digest != st.AppLayerDigests[i] {
			unchanged = false
			break
		}
	}
	if unchanged {
		// The layout already names these deterministic layers. Rewriting the
		// config, manifest, index, and native state would produce identical bytes
		// while invalidating mtimes and making an unchanged run look like work.
		return true, nil
	}
	newDigest, err := spliceNativeLayers(dir, platform, st.AppLayerPositions, st.AppLayerDigests, native)
	if err != nil {
		return false, nil
	}
	digests := make([]string, len(native))
	for i, nl := range native {
		digests[i] = nl.Digest
	}
	if err := saveNativeState(dir, nativeState{
		DepsHash:          st.DepsHash,
		ManifestDigest:    newDigest,
		AppLayerDigests:   digests,
		AppLayerPositions: append([]int(nil), st.AppLayerPositions...),
	}); err != nil {
		return false, err
	}
	return true, nil
}

// ociLayoutDirConfig returns the raw image config blob and its digest for the
// platform's manifest in the layout dir.
func ociLayoutDirConfig(dir, platform string) ([]byte, string, error) {
	manifestData, _, err := ociLayoutDirManifest(dir, platform)
	if err != nil {
		return nil, "", err
	}
	var m ociManifest
	if err := json.Unmarshal(manifestData, &m); err != nil {
		return nil, "", err
	}
	cfgHex, err := digestToHex(m.Config.Digest)
	if err != nil {
		return nil, "", err
	}
	cfgData, err := os.ReadFile(filepath.Join(dir, "blobs", "sha256", cfgHex))
	if err != nil {
		return nil, "", err
	}
	return cfgData, m.Config.Digest, nil
}

// nativeLayerEpoch is the fixed modification time stamped on every entry of a
// native layer. Zeroed times are what make the layer a pure function of the
// input file contents (BuildKit's COPY preserves source mtimes, which defeats
// cross-machine and post-checkout dedup).
var nativeLayerEpoch = time.Unix(0, 0).UTC()

// nativeLayer is one deterministically-built COPY layer, gzip-compressed.
type nativeLayer struct {
	Blob      []byte   // gzip-compressed tar
	Digest    string   // "sha256:<hex>" of Blob (the manifest/blob digest)
	DiffID    string   // "sha256:<hex>" of the uncompressed tar
	FileNames []string // sorted tar entry names (dirs carry a trailing "/")
}

// nativeTarEntry is one pending tar entry, keyed by its (slash-form, no
// leading slash) tar path in the entries map.
type nativeTarEntry struct {
	hdr  tar.Header
	data []byte
}

// buildNativeCopyLayer materializes one Stagefile `copy` entry (from: local)
// as an OCI gzip layer with Docker COPY semantics:
//
//   - dest defaults to Paths[0]; a multi-source copy treats dest as a
//     directory (mirroring codegen's trailing-slash fix-up)
//   - a relative dest resolves against workDir (the image config's
//     WorkingDir; "/" when empty), exactly as COPY does
//   - a directory source copies its CONTENTS into dest
//   - modes are preserved, ownership is numeric root:root, parent directories
//     are synthesized 0755, symlinks stay symlinks
//
// Entries are emitted in sorted path order with fixed mtimes, so identical
// input files yield a byte-identical blob.
func buildNativeCopyLayer(cwd string, entry spec.CopyEntry, workDir string) (*nativeLayer, error) {
	if len(entry.Paths) == 0 {
		return nil, fmt.Errorf("copy entry has no paths")
	}
	dest := entry.Dest
	if dest == "" {
		dest = entry.Paths[0]
	}
	// Mirrors codegen.copyLines: multiple sources make the dest a directory.
	destIsDir := strings.HasSuffix(dest, "/") || len(entry.Paths) > 1
	if !path.IsAbs(dest) {
		wd := workDir
		if wd == "" {
			wd = "/"
		}
		if !path.IsAbs(wd) {
			wd = "/" + wd
		}
		dest = path.Join(wd, dest)
	} else {
		dest = path.Clean(dest)
	}

	entries := map[string]*nativeTarEntry{}

	// tarName converts an absolute in-image path to its tar entry name.
	tarName := func(p string) string {
		return strings.TrimPrefix(path.Clean(p), "/")
	}

	addDirChain := func(imgPath string) {
		// Synthesize each missing ancestor of imgPath as a 0755 root dir.
		clean := tarName(imgPath)
		if clean == "" || clean == "." {
			return
		}
		segs := strings.Split(clean, "/")
		for i := 1; i < len(segs); i++ {
			name := strings.Join(segs[:i], "/") + "/"
			if _, ok := entries[name]; ok {
				continue
			}
			entries[name] = &nativeTarEntry{hdr: tar.Header{
				Typeflag: tar.TypeDir,
				Name:     name,
				Mode:     0o755,
				ModTime:  nativeLayerEpoch,
			}}
		}
	}

	addDir := func(imgPath string, mode os.FileMode) {
		name := tarName(imgPath)
		if name == "" || name == "." {
			return
		}
		addDirChain(imgPath)
		entries[name+"/"] = &nativeTarEntry{hdr: tar.Header{
			Typeflag: tar.TypeDir,
			Name:     name + "/",
			Mode:     int64(mode.Perm()),
			ModTime:  nativeLayerEpoch,
		}}
	}

	addFile := func(absSrc, imgPath string, mode os.FileMode) error {
		data, err := os.ReadFile(absSrc)
		if err != nil {
			return err
		}
		addDirChain(imgPath)
		name := tarName(imgPath)
		entries[name] = &nativeTarEntry{
			hdr: tar.Header{
				Typeflag: tar.TypeReg,
				Name:     name,
				Mode:     int64(mode.Perm()),
				Size:     int64(len(data)),
				ModTime:  nativeLayerEpoch,
			},
			data: data,
		}
		return nil
	}

	addSymlink := func(absSrc, imgPath string) error {
		target, err := os.Readlink(absSrc)
		if err != nil {
			return err
		}
		addDirChain(imgPath)
		name := tarName(imgPath)
		entries[name] = &nativeTarEntry{hdr: tar.Header{
			Typeflag: tar.TypeSymlink,
			Name:     name,
			Linkname: target,
			Mode:     0o777,
			ModTime:  nativeLayerEpoch,
		}}
		return nil
	}

	for _, src := range entry.Paths {
		rel := filepath.ToSlash(filepath.Clean(filepath.FromSlash(src)))
		if path.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../") {
			return nil, fmt.Errorf("copy path %q escapes the project directory", src)
		}
		abs := filepath.Join(cwd, filepath.FromSlash(rel))
		fi, err := os.Lstat(abs)
		if err != nil {
			return nil, fmt.Errorf("copy source %q: %w", src, err)
		}

		switch {
		case fi.IsDir():
			// Docker COPY copies a directory source's CONTENTS into dest.
			addDir(dest, fi.Mode())
			err := filepath.WalkDir(abs, func(p string, d os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				childRel, err := filepath.Rel(abs, p)
				if err != nil {
					return err
				}
				if childRel == "." {
					return nil
				}
				imgPath := path.Join(dest, filepath.ToSlash(childRel))
				info, err := d.Info()
				if err != nil {
					return err
				}
				switch {
				case d.IsDir():
					addDir(imgPath, info.Mode())
					return nil
				case info.Mode()&os.ModeSymlink != 0:
					return addSymlink(p, imgPath)
				case info.Mode().IsRegular():
					return addFile(p, imgPath, info.Mode())
				default:
					return fmt.Errorf("copy source %q: unsupported file type %v", p, info.Mode())
				}
			})
			if err != nil {
				return nil, err
			}

		case fi.Mode()&os.ModeSymlink != 0:
			imgPath := dest
			if destIsDir {
				imgPath = path.Join(dest, path.Base(rel))
			}
			if err := addSymlink(abs, imgPath); err != nil {
				return nil, err
			}

		case fi.Mode().IsRegular():
			imgPath := dest
			if destIsDir {
				imgPath = path.Join(dest, path.Base(rel))
			}
			if err := addFile(abs, imgPath, fi.Mode()); err != nil {
				return nil, err
			}

		default:
			return nil, fmt.Errorf("copy source %q: unsupported file type %v", src, fi.Mode())
		}
	}

	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)

	var rawTar bytes.Buffer
	tw := tar.NewWriter(&rawTar)
	for _, name := range names {
		e := entries[name]
		if err := tw.WriteHeader(&e.hdr); err != nil {
			return nil, fmt.Errorf("writing tar header %q: %w", name, err)
		}
		if len(e.data) > 0 {
			if _, err := tw.Write(e.data); err != nil {
				return nil, fmt.Errorf("writing tar entry %q: %w", name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("finishing layer tar: %w", err)
	}

	var blob bytes.Buffer
	gw, err := gzip.NewWriterLevel(&blob, gzip.DefaultCompression)
	if err != nil {
		return nil, err
	}
	if _, err := gw.Write(rawTar.Bytes()); err != nil {
		return nil, fmt.Errorf("compressing layer: %w", err)
	}
	if err := gw.Close(); err != nil {
		return nil, fmt.Errorf("compressing layer: %w", err)
	}

	rawSum := sha256.Sum256(rawTar.Bytes())
	blobSum := sha256.Sum256(blob.Bytes())
	return &nativeLayer{
		Blob:      blob.Bytes(),
		Digest:    "sha256:" + hex.EncodeToString(blobSum[:]),
		DiffID:    "sha256:" + hex.EncodeToString(rawSum[:]),
		FileNames: names,
	}, nil
}
