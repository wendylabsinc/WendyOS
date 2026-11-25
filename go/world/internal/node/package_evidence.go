package node

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
	"github.com/wendylabsinc/wendy/go/world/internal/geography"
	"github.com/wendylabsinc/wendy/go/world/internal/sitepub"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/internal/worldpackage"
)

// packagePublish is an origin-only operation. Metadata-only site publication
// and replica repair do not require local package bytes; this endpoint does.
func (n *Node) packagePublish(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodPost) {
		return
	}
	if n.cfg.Evidence == nil {
		http.Error(w, "package evidence store unavailable", http.StatusNotFound)
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	id, ok := n.authorize(w, r, "publish", body)
	if !ok {
		return
	}
	if id.Kind != "operator" || !id.Allows("site-publish") || !id.Allows("evidence-read") || !id.Allows("evidence-write") {
		http.Error(w, "package publication requires operator site publication and evidence authority", http.StatusForbidden)
		return
	}
	var record authority.SignedRecord
	if authority.DecodeStrict(body, &record) != nil || record.Component.Key.Source != id.Source || record.Component.Key.Type != sitepub.Type || record.Component.Tombstone {
		http.Error(w, "invalid package publication record", http.StatusBadRequest)
		return
	}
	if _, err := n.verifyRecordContext(r.Context(), record); err != nil {
		http.Error(w, "package record authority rejected", http.StatusForbidden)
		return
	}
	publication, err := sitepub.Validate(record.Component)
	if err != nil {
		http.Error(w, "invalid package site document", http.StatusBadRequest)
		return
	}
	root, err := readPackageRoot(n.cfg.Evidence, publication.PackageSHA256)
	if err != nil {
		http.Error(w, "package root unavailable: "+err.Error(), http.StatusConflict)
		return
	}
	manifest, err := worldpackage.Inspect(root)
	if err != nil || manifest.SHA256 != publication.PackageSHA256 || manifest.Site != publication.Site {
		http.Error(w, "package root differs from site publication", http.StatusBadRequest)
		return
	}
	placement, err := geography.DecodeSite(publication.Document)
	if err != nil {
		http.Error(w, "invalid package placement", http.StatusBadRequest)
		return
	}
	frameMatches := placement.Config().Root.Revision == manifest.FrameRevision
	for _, frame := range placement.Config().Frames {
		frameMatches = frameMatches || frame.Ref.Revision == manifest.FrameRevision
	}
	if !frameMatches {
		http.Error(w, "package frame revision absent from site", http.StatusBadRequest)
		return
	}
	refs := []evidence.Reference{{SHA256: manifest.SHA256, Size: int64(len(root)), ContentType: "application/octet-stream"}}
	total := int64(len(root))
	for _, file := range manifest.Files {
		// The package schemas bind the MJCF digest but do not declare its size.
		// Read verified local metadata, then recheck it atomically with the hold.
		if file.Size == 0 {
			part, err := n.cfg.Evidence.Get(evidence.GetRequest{SHA256: file.SHA256, Length: 1})
			if err != nil || part.Size < 1 || part.Size > worldpackage.MaxModelBytes || part.ContentType != "application/octet-stream" {
				http.Error(w, "package model unavailable or invalid", http.StatusConflict)
				return
			}
			file.Size = part.Size
		}
		total += file.Size
		if total > worldpackage.MaxTotalBytes {
			http.Error(w, "package exceeds total size bound", http.StatusBadRequest)
			return
		}
		refs = append(refs, evidence.Reference{SHA256: file.SHA256, Size: file.Size, ContentType: "application/octet-stream"})
	}
	until := record.Component.Timing.SampleTime.Add(record.Component.TTL).UnixMilli()
	// Bind the hold to the exact instruction. An older or concurrent publication
	// of the same site document cannot shorten a different record's protection.
	canonical, _ := json.Marshal(record)
	holdID := sha256.Sum256(canonical)
	if _, err := n.cfg.Evidence.ReplaceHold(evidence.HoldPackage, id.Principal, hex.EncodeToString(holdID[:]), refs, until); err != nil {
		status := http.StatusConflict
		if errors.Is(err, evidence.ErrQuota) {
			status = http.StatusInsufficientStorage
		}
		http.Error(w, "package evidence hold rejected: "+err.Error(), status)
		return
	}
	// Authority and publisher epoch are rechecked after the durable hold. A
	// failed/uncertain publication may leave a bounded hold through this same
	// original deadline; retrying cannot renew the signed record.
	result, err := n.applyContext(r.Context(), record, true)
	if err != nil {
		http.Error(w, "package publication rejected: "+err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Result    world.Result    `json:"result"`
		Component world.Component `json:"component"`
	}{result, record.Component})
}

func readPackageRoot(store *evidence.Store, digest string) ([]byte, error) {
	var data bytes.Buffer
	var first evidence.GetResponse
	for offset := int64(0); ; {
		part, err := store.Get(evidence.GetRequest{SHA256: digest, Offset: offset, Length: evidence.MaxChunk})
		if err != nil {
			return nil, err
		}
		if part.ContentType != "application/octet-stream" || part.Size < 1 || part.Size > worldpackage.MaxRootBytes || part.Offset != offset || len(part.Data) == 0 || int64(len(part.Data)) > part.Size-offset {
			return nil, errors.New("invalid package root metadata")
		}
		if offset == 0 {
			first = part
		} else if part.Size != first.Size || part.RetainUntilUnixMS != first.RetainUntilUnixMS {
			return nil, errors.New("package root changed during read")
		}
		data.Write(part.Data)
		offset += int64(len(part.Data))
		if offset == part.Size {
			sum := sha256.Sum256(data.Bytes())
			if hex.EncodeToString(sum[:]) != digest {
				return nil, errors.New("package root digest mismatch")
			}
			return data.Bytes(), nil
		}
	}
}
