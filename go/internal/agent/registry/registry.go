// Package registry provides an embedded OCI Distribution registry backed by
// containerd's content store. It is a direct integration of the standalone
// containerd-registry (github.com/wendylabsinc/containerd-registry) adapted
// for containerd v2 APIs.
package registry

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cuelabs.dev/go/oci/ociregistry"
	"cuelabs.dev/go/oci/ociregistry/ociserver"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/errdefs"
	"github.com/distribution/reference"
	"github.com/google/uuid"
	digest "github.com/opencontainers/go-digest"
	"go.uber.org/zap"
)

// Server is the embedded OCI registry HTTP server.
type Server struct {
	httpServer   *http.Server
	client       *containerd.Client
	logger       *zap.Logger
	shutdownOnce sync.Once
}

// Shutdown gracefully stops the registry server and releases the containerd client.
// Safe to call multiple times; subsequent calls are no-ops.
func (s *Server) Shutdown(ctx context.Context) error {
	var err error
	s.shutdownOnce.Do(func() {
		err = s.httpServer.Shutdown(ctx)
		s.client.Close()
	})
	return err
}

// Start creates a new OCI registry HTTP server backed by the given containerd
// client and starts listening on listenAddr. When tlsConfig is non-nil the
// server is served over HTTPS. The server shuts down gracefully when ctx is
// cancelled.
func Start(ctx context.Context, containerdAddr, listenAddr string, logger *zap.Logger, tlsConfig *tls.Config) (*Server, error) {
	client, err := containerd.New(containerdAddr, containerd.WithDefaultNamespace("default"))
	if err != nil {
		return nil, fmt.Errorf("connecting to containerd for registry: %w", err)
	}

	// Derive the host:port prefix from the listen address so that images are
	// registered in containerd with the same name clients use to pull them
	// (e.g. "localhost:5000/myapp:latest").
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("parsing registry listen address %q: %w", listenAddr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	imagePrefix := net.JoinHostPort(host, port) + "/"

	reg := &containerdRegistry{
		client:              client,
		imagePrefix:         imagePrefix,
		blobLeaseExpiration: 15 * time.Minute,
		manifestSizeLimit:   4 * 1024 * 1024, // 4 MiB
	}

	var backend ociregistry.Interface = safeDeleteRegistry{
		Interface: reg,
	}

	ociHandler := ociserver.New(backend, nil)

	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", readyzHandler(client, logger))
	mux.Handle("/", ociHandler)

	handler := loggingMiddleware(logger)(securityHeadersMiddleware(mux))

	tcpLis, err := net.Listen("tcp", listenAddr)
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("listening on %s for registry: %w", listenAddr, err)
	}

	var serveListener net.Listener = tcpLis
	if tlsConfig != nil {
		serveListener = tls.NewListener(tcpLis, tlsConfig)
	}

	srv := &http.Server{
		Handler:        handler,
		ReadTimeout:    5 * time.Minute,
		WriteTimeout:   5 * time.Minute,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	s := &Server{httpServer: srv, client: client, logger: logger}

	scheme := "HTTP"
	if tlsConfig != nil {
		scheme = "HTTPS"
	}
	go func() {
		logger.Info("Dev registry listening", zap.String("address", listenAddr), zap.String("scheme", scheme))
		if err := srv.Serve(serveListener); err != nil && err != http.ErrServerClosed {
			logger.Error("Dev registry server error", zap.Error(err))
		}
	}()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.Shutdown(shutdownCtx)
	}()

	return s, nil
}

// ---------------------------------------------------------------------------
// HTTP middleware & handlers
// ---------------------------------------------------------------------------

type responseWriter struct {
	http.ResponseWriter
	status      int
	bytes       atomic.Int64
	wroteHeader bool
}

func (rw *responseWriter) WriteHeader(status int) {
	if !rw.wroteHeader {
		rw.status = status
		rw.ResponseWriter.WriteHeader(status)
		rw.wroteHeader = true
	}
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	if !rw.wroteHeader {
		rw.WriteHeader(http.StatusOK)
	}
	n, err := rw.ResponseWriter.Write(b)
	rw.bytes.Add(int64(n))
	return n, err
}

func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func loggingMiddleware(logger *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			wrapped := &responseWriter{ResponseWriter: w, status: 200}
			next.ServeHTTP(wrapped, r)
			duration := time.Since(start)

			if wrapped.status >= 500 {
				logger.Error("registry request",
					zap.String("method", r.Method), zap.String("path", r.URL.Path),
					zap.Int("status", wrapped.status), zap.Duration("duration", duration))
			} else if wrapped.status >= 400 {
				logger.Warn("registry request",
					zap.String("method", r.Method), zap.String("path", r.URL.Path),
					zap.Int("status", wrapped.status), zap.Duration("duration", duration))
			} else {
				logger.Debug("registry request",
					zap.String("method", r.Method), zap.String("path", r.URL.Path),
					zap.Int("status", wrapped.status), zap.Duration("duration", duration))
			}
		})
	}
}

func readyzHandler(client *containerd.Client, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		if _, err := client.Version(ctx); err != nil {
			logger.Error("readyz: containerd not ready", zap.Error(err))
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintln(w, "containerd not ready")
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "OK")
	}
}

// ---------------------------------------------------------------------------
// OCI registry implementation backed by containerd
// ---------------------------------------------------------------------------

type containerdRegistry struct {
	*ociregistry.Funcs
	client *containerd.Client

	// imagePrefix is prepended to repo names when registering images in
	// containerd's image store (e.g. "localhost:5000/") so that the names
	// match what clients pass to GetImage / Pull.
	imagePrefix string

	blobLeaseExpiration time.Duration
	manifestSizeLimit   int64
	maxBlobSize         int64
}

// imageName returns the full containerd image name for a repo and tag,
// including the registry prefix (e.g. "localhost:5000/myapp:latest").
func (r containerdRegistry) imageName(repo, tag string) string {
	return r.imagePrefix + repo + ":" + tag
}

func (r containerdRegistry) Repositories(ctx context.Context, startAfter string) ociregistry.Seq[string] {
	is := r.client.ImageService()
	imgs, err := is.List(ctx)
	if err != nil {
		return ociregistry.ErrorSeq[string](err)
	}

	seen := make(map[string]bool)
	var names []string
	for _, img := range imgs {
		// Only list images managed by this registry (matching the prefix).
		if !strings.HasPrefix(img.Name, r.imagePrefix) {
			continue
		}
		// Strip prefix and tag/digest to get the bare repo name.
		nameWithoutPrefix := strings.TrimPrefix(img.Name, r.imagePrefix)
		// Strip digest references (e.g. "repo@sha256:...") before tags.
		if i := strings.IndexByte(nameWithoutPrefix, '@'); i >= 0 {
			nameWithoutPrefix = nameWithoutPrefix[:i]
		}
		repo, _, _ := strings.Cut(nameWithoutPrefix, ":")
		if repo == "" || seen[repo] {
			continue
		}
		seen[repo] = true
		names = append(names, repo)
	}

	sort.Slice(names, func(i, j int) bool { return strings.ToLower(names[i]) < strings.ToLower(names[j]) })

	if startAfter != "" {
		startAfterLower := strings.ToLower(startAfter)
		first := sort.Search(len(names), func(i int) bool { return strings.ToLower(names[i]) > startAfterLower })
		names = names[first:]
	}

	return ociregistry.SliceSeq[string](names)
}

func (r containerdRegistry) Tags(ctx context.Context, repo string, startAfter string) ociregistry.Seq[string] {
	is := r.client.ImageService()
	prefixedRepo := r.imagePrefix + repo
	imgs, err := is.List(ctx, "name~="+strconv.Quote("^"+escapeRegex(prefixedRepo)+":"))
	if err != nil {
		return ociregistry.ErrorSeq[string](err)
	}

	var tags []string
	for _, img := range imgs {
		ref, err := reference.Parse(img.Name)
		if err != nil {
			continue
		}
		if _, ok := ref.(reference.Digested); ok {
			continue
		}
		named, ok := ref.(reference.Named)
		if !ok || named.Name() != prefixedRepo {
			continue
		}
		if tagged, ok := ref.(reference.Tagged); ok {
			tags = append(tags, tagged.Tag())
		}
	}

	sort.Slice(tags, func(i, j int) bool { return strings.ToLower(tags[i]) < strings.ToLower(tags[j]) })

	if startAfter != "" {
		startAfterLower := strings.ToLower(startAfter)
		first := sort.Search(len(tags), func(i int) bool { return strings.ToLower(tags[i]) > startAfterLower })
		tags = tags[first:]
	}

	return ociregistry.SliceSeq[string](tags)
}

// escapeRegex escapes special regex characters for use in containerd image filters.
func escapeRegex(s string) string {
	const special = `\.+*?()|[]{}^$`
	var b strings.Builder
	for _, c := range s {
		if strings.ContainsRune(special, c) {
			b.WriteRune('\\')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Blob reader
// ---------------------------------------------------------------------------

type containerdBlobReader struct {
	client   *containerd.Client
	ctx      context.Context
	desc     ociregistry.Descriptor
	readerAt content.ReaderAt
	reader   io.Reader
}

func (br *containerdBlobReader) validate() error {
	info, err := br.client.ContentStore().Info(br.ctx, br.desc.Digest)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return ociregistry.ErrBlobUnknown
		}
		return err
	}
	if br.desc.Size == 0 && info.Size != 0 {
		br.desc.Size = info.Size
	}
	if br.desc.MediaType == "" {
		br.desc.MediaType = "application/octet-stream"
	}
	return nil
}

func (br *containerdBlobReader) ensureReaderAt() (content.ReaderAt, error) {
	if br.readerAt == nil {
		var err error
		br.readerAt, err = br.client.ContentStore().ReaderAt(br.ctx, br.desc)
		if err != nil {
			return nil, err
		}
	}
	return br.readerAt, nil
}

func (br *containerdBlobReader) ensureReader() (io.Reader, error) {
	if br.reader == nil {
		ra, err := br.ensureReaderAt()
		if err != nil {
			return nil, err
		}
		br.reader = content.NewReader(ra)
	}
	return br.reader, nil
}

func (br *containerdBlobReader) Read(p []byte) (int, error) {
	r, err := br.ensureReader()
	if err != nil {
		return 0, err
	}
	return r.Read(p)
}

func (br *containerdBlobReader) Descriptor() ociregistry.Descriptor {
	return br.desc
}

func (br *containerdBlobReader) Close() error {
	if br.readerAt != nil {
		return br.readerAt.Close()
	}
	return nil
}

func newContainerdBlobReaderFromDescriptor(ctx context.Context, client *containerd.Client, desc ociregistry.Descriptor) (*containerdBlobReader, error) {
	br := &containerdBlobReader{client: client, ctx: ctx, desc: desc}
	if err := br.validate(); err != nil {
		br.Close()
		return nil, err
	}
	return br, nil
}

func newContainerdBlobReaderFromDigest(ctx context.Context, client *containerd.Client, d ociregistry.Digest) (*containerdBlobReader, error) {
	return newContainerdBlobReaderFromDescriptor(ctx, client, ociregistry.Descriptor{Digest: d})
}

// ---------------------------------------------------------------------------
// Read operations
// ---------------------------------------------------------------------------

func (r containerdRegistry) GetBlob(ctx context.Context, repo string, d ociregistry.Digest) (ociregistry.BlobReader, error) {
	return newContainerdBlobReaderFromDigest(ctx, r.client, d)
}

func (r containerdRegistry) GetBlobRange(ctx context.Context, repo string, d ociregistry.Digest, offset0, offset1 int64) (ociregistry.BlobReader, error) {
	br, err := newContainerdBlobReaderFromDigest(ctx, r.client, d)
	if err != nil {
		return nil, err
	}
	ra, err := br.ensureReaderAt()
	if err != nil {
		br.Close()
		return nil, err
	}

	var size int64
	if offset1 >= 0 && offset1 < offset0 {
		br.Close()
		return nil, ociregistry.NewHTTPError(errors.New("invalid range: end before start"), http.StatusRequestedRangeNotSatisfiable, nil, nil)
	}
	if offset1 < 0 || offset0 >= br.desc.Size {
		if offset0 < 0 {
			offset0 = 0
		}
		size = br.desc.Size - offset0
	} else {
		size = offset1 - offset0
		if offset0+size > br.desc.Size {
			size = br.desc.Size - offset0
		}
	}
	br.reader = io.NewSectionReader(ra, offset0, size)
	return br, nil
}

func (r containerdRegistry) GetManifest(ctx context.Context, repo string, d ociregistry.Digest) (ociregistry.BlobReader, error) {
	desc := ociregistry.Descriptor{Digest: d}
	ra, err := r.client.ContentStore().ReaderAt(ctx, desc)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, ociregistry.ErrManifestUnknown
		}
		return nil, err
	}
	defer ra.Close()

	desc.Size = ra.Size()
	reader := io.LimitReader(content.NewReader(ra), r.manifestSizeLimit)

	mediaTypeWrapper := struct {
		MediaType string `json:"mediaType"`
	}{}
	if err := json.NewDecoder(reader).Decode(&mediaTypeWrapper); err != nil {
		return nil, err
	}
	if mediaTypeWrapper.MediaType == "" {
		return nil, errors.New("failed to parse mediaType from manifest")
	}
	if strings.ContainsAny(mediaTypeWrapper.MediaType, "\r\n") {
		return nil, errors.New("invalid mediaType: contains control characters")
	}
	desc.MediaType = mediaTypeWrapper.MediaType

	br, err := newContainerdBlobReaderFromDescriptor(ctx, r.client, desc)
	if err == ociregistry.ErrBlobUnknown {
		return nil, ociregistry.ErrManifestUnknown
	}
	return br, err
}

func (r containerdRegistry) GetTag(ctx context.Context, repo string, tagName string) (ociregistry.BlobReader, error) {
	is := r.client.ImageService()
	img, err := is.Get(ctx, r.imageName(repo, tagName))
	if err != nil {
		if errdefs.IsNotFound(err) {
			if repo == "sha256" && len(tagName) == 64 {
				if d, err := digest.Parse(repo + ":" + tagName); err == nil {
					if br, err := r.GetManifest(ctx, repo, d); err == nil {
						return br, nil
					}
				}
			}
			return nil, ociregistry.ErrManifestUnknown
		}
		return nil, err
	}
	return newContainerdBlobReaderFromDescriptor(ctx, r.client, img.Target)
}

func (r containerdRegistry) ResolveBlob(ctx context.Context, repo string, d ociregistry.Digest) (ociregistry.Descriptor, error) {
	br, err := r.GetBlob(ctx, repo, d)
	if err != nil {
		return ociregistry.Descriptor{}, err
	}
	defer br.Close()
	return br.Descriptor(), nil
}

func (r containerdRegistry) ResolveManifest(ctx context.Context, repo string, d ociregistry.Digest) (ociregistry.Descriptor, error) {
	br, err := r.GetManifest(ctx, repo, d)
	if err != nil {
		return ociregistry.Descriptor{}, err
	}
	defer br.Close()
	return br.Descriptor(), nil
}

func (r containerdRegistry) ResolveTag(ctx context.Context, repo string, tagName string) (ociregistry.Descriptor, error) {
	br, err := r.GetTag(ctx, repo, tagName)
	if err != nil {
		return ociregistry.Descriptor{}, err
	}
	defer br.Close()
	return br.Descriptor(), nil
}

// ---------------------------------------------------------------------------
// Delete operations (wrapped by safeDeleteRegistry)
// ---------------------------------------------------------------------------

func (r containerdRegistry) DeleteBlob(ctx context.Context, repo string, d ociregistry.Digest) error {
	return r.client.ContentStore().Delete(ctx, d)
}

func (r containerdRegistry) DeleteManifest(ctx context.Context, repo string, d ociregistry.Digest) error {
	return r.client.ContentStore().Delete(ctx, d)
}

func (r containerdRegistry) DeleteTag(ctx context.Context, repo string, name string) error {
	return r.client.ImageService().Delete(ctx, r.imageName(repo, name))
}

type safeDeleteRegistry struct {
	ociregistry.Interface
	allowDelete bool
}

func (s safeDeleteRegistry) DeleteBlob(ctx context.Context, repo string, d ociregistry.Digest) error {
	if !s.allowDelete {
		return ociregistry.ErrDenied
	}
	return s.Interface.DeleteBlob(ctx, repo, d)
}

func (s safeDeleteRegistry) DeleteManifest(ctx context.Context, repo string, d ociregistry.Digest) error {
	if !s.allowDelete {
		return ociregistry.ErrDenied
	}
	return s.Interface.DeleteManifest(ctx, repo, d)
}

func (s safeDeleteRegistry) DeleteTag(ctx context.Context, repo string, name string) error {
	if !s.allowDelete {
		return ociregistry.ErrDenied
	}
	return s.Interface.DeleteTag(ctx, repo, name)
}

// ---------------------------------------------------------------------------
// Write operations
// ---------------------------------------------------------------------------

func (r containerdRegistry) PushBlob(ctx context.Context, repo string, desc ociregistry.Descriptor, reader io.Reader) (ociregistry.Descriptor, error) {
	if r.maxBlobSize > 0 && desc.Size > r.maxBlobSize {
		return ociregistry.Descriptor{}, ociregistry.NewHTTPError(
			fmt.Errorf("blob size %d exceeds maximum allowed size %d", desc.Size, r.maxBlobSize),
			http.StatusRequestEntityTooLarge, nil, nil,
		)
	}

	cs := r.client.ContentStore()
	// The lease protects the blob from GC until a manifest references it.
	// Let the lease expire naturally rather than deleting it immediately.
	ctx, _, err := r.client.WithLease(ctx, leases.WithExpiration(r.blobLeaseExpiration))
	if err != nil {
		return ociregistry.Descriptor{}, err
	}

	reader = io.LimitReader(reader, desc.Size+1)
	ingestRef := string(desc.Digest)

	if err := cs.Abort(ctx, ingestRef); err != nil && !errdefs.IsNotFound(err) {
		return ociregistry.Descriptor{}, err
	}

	if err := content.WriteBlob(ctx, cs, ingestRef, reader, desc); err != nil {
		_ = cs.Abort(ctx, ingestRef)
		return ociregistry.Descriptor{}, err
	}

	return desc, nil
}

type containerdBlobWriter struct {
	ctx       context.Context
	cs        content.Store
	id        string
	chunkSize int
	content.Writer

	closedStatus *content.Status
}

func (bw *containerdBlobWriter) cacheStatus() error {
	status, err := bw.Writer.Status()
	if err == nil {
		bw.closedStatus = &status
	}
	return err
}

func (bw *containerdBlobWriter) Close() error {
	return errors.Join(bw.cacheStatus(), bw.Writer.Close())
}

func (bw *containerdBlobWriter) Size() int64 {
	if bw.closedStatus != nil {
		return bw.closedStatus.Offset
	}
	status, err := bw.Writer.Status()
	if err != nil {
		return 0
	}
	return status.Offset
}

func (bw *containerdBlobWriter) ID() string {
	return bw.id
}

func (bw *containerdBlobWriter) ChunkSize() int {
	if bw.chunkSize < 1 {
		return 1
	}
	return bw.chunkSize
}

func (bw *containerdBlobWriter) Commit(d ociregistry.Digest) (ociregistry.Descriptor, error) {
	if err := bw.cacheStatus(); err != nil {
		return ociregistry.Descriptor{}, err
	}
	if err := bw.Writer.Commit(bw.ctx, 0, d); err != nil && !errdefs.IsAlreadyExists(err) {
		return ociregistry.Descriptor{}, err
	}
	return ociregistry.Descriptor{
		Digest:    d,
		Size:      bw.Size(),
		MediaType: "application/octet-stream",
	}, nil
}

func (bw *containerdBlobWriter) Cancel() error {
	if err := bw.Close(); err != nil {
		return err
	}
	return bw.cs.Abort(bw.ctx, bw.id)
}

func (r containerdRegistry) PushBlobChunked(ctx context.Context, repo string, chunkSize int) (ociregistry.BlobWriter, error) {
	return r.PushBlobChunkedResume(ctx, repo, "", 0, chunkSize)
}

func (r containerdRegistry) PushBlobChunkedResume(ctx context.Context, repo, id string, offset int64, chunkSize int) (ociregistry.BlobWriter, error) {
	if offset == 0 && chunkSize == 0 {
		offset = -1
	}

	cs := r.client.ContentStore()

	if id == "" {
		u, err := uuid.NewRandom()
		if err != nil {
			return nil, err
		}
		id = u.String()
	}

	// The lease protects the blob from GC until a manifest references it.
	// Let the lease expire naturally rather than deleting it on writer close.
	ctx, _, err := r.client.WithLease(ctx, leases.WithExpiration(r.blobLeaseExpiration))
	if err != nil {
		return nil, err
	}

	writer, err := content.OpenWriter(ctx, cs, content.WithRef(id))
	if err != nil {
		return nil, err
	}

	if offset != -1 {
		if status, err := writer.Status(); err != nil {
			return nil, err
		} else if offset != status.Offset {
			return nil, ociregistry.NewHTTPError(
				errors.New("offset ("+strconv.FormatInt(offset, 10)+") must match previous value ("+strconv.FormatInt(status.Offset, 10)+")"),
				http.StatusRequestedRangeNotSatisfiable, nil, nil,
			)
		}
	}

	return &containerdBlobWriter{
		ctx:       ctx,
		cs:        cs,
		id:        id,
		chunkSize: chunkSize,
		Writer:    writer,
	}, nil
}

func (r containerdRegistry) MountBlob(ctx context.Context, fromRepo, toRepo string, d ociregistry.Digest) (ociregistry.Descriptor, error) {
	return r.ResolveBlob(ctx, toRepo, d)
}

func (r containerdRegistry) PushManifest(ctx context.Context, repo string, tag string, contents []byte, mediaType string) (ociregistry.Descriptor, error) {
	desc := ociregistry.Descriptor{
		Digest:    digest.FromBytes(contents),
		Size:      int64(len(contents)),
		MediaType: mediaType,
	}

	manifestChildren := struct {
		Manifests []ociregistry.Descriptor `json:"manifests"`
		Config    *ociregistry.Descriptor  `json:"config"`
		Layers    []ociregistry.Descriptor `json:"layers"`
		Subject   *ociregistry.Descriptor  `json:"subject"`
	}{}
	if err := json.Unmarshal(contents, &manifestChildren); err != nil {
		return ociregistry.Descriptor{}, err
	}

	labelMappings := map[string]*ociregistry.Descriptor{
		"config":  manifestChildren.Config,
		"subject": manifestChildren.Subject,
	}
	for prefix, list := range map[string][]ociregistry.Descriptor{
		"m": manifestChildren.Manifests,
		"l": manifestChildren.Layers,
	} {
		for i, d := range list {
			d := d
			labelMappings[prefix+"."+strconv.Itoa(i)] = &d
		}
	}

	labels := map[string]string{}
	for field, d := range labelMappings {
		if d != nil {
			if _, err := digest.Parse(string(d.Digest)); err != nil {
				return ociregistry.Descriptor{}, fmt.Errorf("invalid child digest in manifest field %q: %w", field, err)
			}
			labels["containerd.io/gc.ref.content."+field] = string(d.Digest)
		}
	}
	if manifestChildren.Subject != nil {
		labels["containerd.io/gc.bref.content.subject"] = string(manifestChildren.Subject.Digest)
	}

	ctx, _, err := r.client.WithLease(ctx, leases.WithExpiration(r.blobLeaseExpiration))
	if err != nil {
		return ociregistry.Descriptor{}, err
	}

	cs := r.client.ContentStore()
	ingestRef := string(desc.Digest)
	if err := cs.Abort(ctx, ingestRef); err != nil && !errdefs.IsNotFound(err) {
		return ociregistry.Descriptor{}, err
	}
	if err := content.WriteBlob(ctx, cs, ingestRef, bytes.NewReader(contents), desc, content.WithLabels(labels)); err != nil {
		return ociregistry.Descriptor{}, err
	}

	if tag != "" {
		is := r.client.ImageService()
		img := images.Image{
			Name:   r.imageName(repo, tag),
			Target: desc,
		}
		_, err := is.Update(ctx, img, "target")
		if err != nil {
			if !errdefs.IsNotFound(err) {
				return desc, err
			}
			_, err = is.Create(ctx, img)
			if err != nil {
				return desc, err
			}
		}
	}

	return desc, nil
}
