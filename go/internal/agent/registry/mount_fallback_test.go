package registry

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"cuelabs.dev/go/oci/ociregistry"
	"cuelabs.dev/go/oci/ociregistry/ocimem"
	digest "github.com/opencontainers/go-digest"
)

type mountTestBackend struct {
	ociregistry.Interface
	mountErr    error
	uploadErr   error
	mountCalls  int
	uploadCalls int
}

func (b *mountTestBackend) MountBlob(ctx context.Context, from, to string, d ociregistry.Digest) (ociregistry.Descriptor, error) {
	b.mountCalls++
	if b.mountErr != nil {
		return ociregistry.Descriptor{}, b.mountErr
	}
	return b.Interface.MountBlob(ctx, from, to, d)
}

func (b *mountTestBackend) PushBlobChunked(ctx context.Context, repo string, size int) (ociregistry.BlobWriter, error) {
	b.uploadCalls++
	if b.uploadErr != nil {
		return nil, b.uploadErr
	}
	return b.Interface.PushBlobChunked(ctx, repo, size)
}

func TestMissingMountFallsBackToUsableUpload(t *testing.T) {
	for _, extraDigest := range []bool{false, true} {
		t.Run(fmt.Sprintf("extra_digest=%t", extraDigest), func(t *testing.T) {
			backend := &mountTestBackend{Interface: ocimem.New(), mountErr: fmt.Errorf("lookup: %w", ociregistry.ErrBlobUnknown)}
			handler := newOCIHandler(backend)
			payload := []byte("missing blob uploaded after a failed mount")
			d := digest.FromBytes(payload)
			path := "/v2/destination/blobs/uploads/?mount=" + string(d) + "&from=source"
			if extraDigest {
				path += "&digest=" + string(d)
			}
			mount := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, path, nil)
			originalQuery := req.URL.RawQuery
			handler.ServeHTTP(mount, req)
			if mount.Code != http.StatusAccepted || backend.mountCalls != 1 || backend.uploadCalls != 1 {
				t.Fatalf("mount: status=%d mounts=%d uploads=%d body=%s", mount.Code, backend.mountCalls, backend.uploadCalls, mount.Body.String())
			}
			if req.URL.RawQuery != originalQuery {
				t.Fatal("fallback mutated the original request")
			}
			location := mount.Header().Get("Location")
			if location == "" {
				t.Fatal("fallback supplied no upload Location")
			}
			uploadURL, err := url.Parse(location)
			if err != nil {
				t.Fatal(err)
			}
			q := uploadURL.Query()
			q.Set("digest", string(d))
			uploadURL.RawQuery = q.Encode()
			commit := httptest.NewRecorder()
			handler.ServeHTTP(commit, httptest.NewRequest(http.MethodPut, uploadURL.String(), bytes.NewReader(payload)))
			if commit.Code != http.StatusCreated {
				t.Fatalf("commit: status=%d body=%s", commit.Code, commit.Body.String())
			}
			read := httptest.NewRecorder()
			handler.ServeHTTP(read, httptest.NewRequest(http.MethodGet, "/v2/destination/blobs/"+string(d), nil))
			if read.Code != http.StatusOK || !bytes.Equal(read.Body.Bytes(), payload) {
				t.Fatalf("read uploaded blob: status=%d body=%q", read.Code, read.Body.Bytes())
			}
		})
	}
}

func TestExistingMountDoesNotStartUpload(t *testing.T) {
	backend := &mountTestBackend{Interface: ocimem.New()}
	payload := []byte("already stored blob")
	d := ociregistry.Descriptor{Digest: digest.FromBytes(payload), Size: int64(len(payload)), MediaType: "application/octet-stream"}
	if _, err := backend.PushBlob(context.Background(), "source", d, bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	newOCIHandler(backend).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v2/destination/blobs/uploads/?mount="+string(d.Digest)+"&from=source", nil))
	if response.Code != http.StatusCreated || backend.uploadCalls != 0 {
		t.Fatalf("mount: status=%d uploads=%d body=%s", response.Code, backend.uploadCalls, response.Body.String())
	}
	reader, err := backend.GetBlob(context.Background(), "destination", d.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	got, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("mounted blob=%q err=%v", got, err)
	}
}

func TestMountFallbackPreservesOtherErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
	}{
		{"denied", ociregistry.ErrDenied, http.StatusForbidden},
		{"unauthorized", ociregistry.ErrUnauthorized, http.StatusUnauthorized},
		{"unsupported", ociregistry.ErrUnsupported, http.StatusBadRequest},
		{"other_not_found", ociregistry.ErrNameUnknown, http.StatusNotFound},
		{"backend_failure", fmt.Errorf("storage unavailable"), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &mountTestBackend{Interface: ocimem.New(), mountErr: tc.err}
			response := httptest.NewRecorder()
			newOCIHandler(backend).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v2/destination/blobs/uploads/?mount="+string(digest.FromString("missing"))+"&from=source", nil))
			if response.Code != tc.status || backend.uploadCalls != 0 {
				t.Fatalf("status=%d uploads=%d body=%s", response.Code, backend.uploadCalls, response.Body.String())
			}
		})
	}
}

func TestMountFallbackRetainsRequestValidationAndNormalUploads(t *testing.T) {
	for _, tc := range []struct {
		name, method, path string
		status, uploads    int
	}{
		{"invalid_digest", http.MethodPost, "/v2/destination/blobs/uploads/?mount=invalid&from=source", http.StatusBadRequest, 0},
		{"invalid_source", http.MethodPost, "/v2/destination/blobs/uploads/?mount=" + string(digest.FromString("missing")) + "&from=INVALID", http.StatusBadRequest, 0},
		{"wrong_method", http.MethodGet, "/v2/destination/blobs/uploads/?mount=" + string(digest.FromString("missing")) + "&from=source", http.StatusMethodNotAllowed, 0},
		{"normal_upload", http.MethodPost, "/v2/destination/blobs/uploads/", http.StatusAccepted, 1},
		{"missing_blob_read", http.MethodGet, "/v2/destination/blobs/" + string(digest.FromString("missing")), http.StatusNotFound, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &mountTestBackend{Interface: ocimem.New(), mountErr: ociregistry.ErrBlobUnknown}
			response := httptest.NewRecorder()
			newOCIHandler(backend).ServeHTTP(response, httptest.NewRequest(tc.method, tc.path, nil))
			if response.Code != tc.status || backend.mountCalls != 0 || backend.uploadCalls != tc.uploads {
				t.Fatalf("status=%d mounts=%d uploads=%d body=%s", response.Code, backend.mountCalls, backend.uploadCalls, response.Body.String())
			}
		})
	}
}

func TestMissingMountPreservesUploadFailure(t *testing.T) {
	backend := &mountTestBackend{Interface: ocimem.New(), mountErr: ociregistry.ErrBlobUnknown, uploadErr: fmt.Errorf("cannot create upload")}
	response := httptest.NewRecorder()
	newOCIHandler(backend).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v2/destination/blobs/uploads/?mount="+string(digest.FromString("missing"))+"&from=source", nil))
	if response.Code != http.StatusInternalServerError || backend.mountCalls != 1 || backend.uploadCalls != 1 {
		t.Fatalf("status=%d mounts=%d uploads=%d body=%s", response.Code, backend.mountCalls, backend.uploadCalls, response.Body.String())
	}
}
