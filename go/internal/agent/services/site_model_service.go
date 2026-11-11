package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

const SiteModelMaxBytes = 256 << 20
const siteModelChunkBytes = 256 << 10

var siteModelID = regexp.MustCompile(`^[a-f0-9]{64}$`)

// SiteModelService stores opaque immutable archives beside the episode store,
// on the data partition selected by the agent. No client-supplied file paths.
type SiteModelService struct {
	pb.UnimplementedSiteModelServiceServer
	root   string
	upload sync.Mutex
}

func NewSiteModelService(root string) *SiteModelService { return &SiteModelService{root: root} }
func authorizeSiteModels(ctx context.Context) error {
	if p, ok := peer.FromContext(ctx); ok && onLocalAdminSocket(p) {
		return nil
	}
	_, err := userIdentityFromContext(ctx, "accessing saved site models")
	return err
}
func (s *SiteModelService) List(ctx context.Context, _ *pb.SiteModelListRequest) (*pb.SiteModelListResponse, error) {
	if err := authorizeSiteModels(ctx); err != nil {
		return nil, err
	}
	result := &pb.SiteModelListResponse{}
	entries, err := os.ReadDir(s.root)
	if os.IsNotExist(err) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !siteModelID.MatchString(id) || entry.Name() != id+".json" || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		stat, statErr := entry.Info()
		if statErr != nil || !stat.Mode().IsRegular() || stat.Size() > 4096 {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.root, entry.Name()))
		if err != nil {
			continue
		}
		var info pb.SiteModelInfo
		if len(data) > 4096 || json.Unmarshal(data, &info) != nil || info.Id != id {
			continue
		}
		file, err := os.Lstat(filepath.Join(s.root, id+".wendysite"))
		if err == nil && file.Mode().IsRegular() && file.Size() == info.Size {
			result.Models = append(result.Models, &info)
		}
	}
	return result, nil
}
func (s *SiteModelService) Upload(stream grpc.ClientStreamingServer[pb.SiteModelUploadRequest, pb.SiteModelInfo]) error {
	if err := authorizeSiteModels(stream.Context()); err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	info := first.Info
	if info == nil || !siteModelID.MatchString(info.Id) || strings.TrimSpace(info.Name) == "" || len(info.Name) > 512 || info.Size < 16 || info.Size > SiteModelMaxBytes || len(first.Data) != 0 {
		return status.Error(codes.InvalidArgument, "invalid site model metadata")
	}
	if !s.upload.TryLock() {
		return status.Error(codes.ResourceExhausted, "another model is being saved; try again")
	}
	defer s.upload.Unlock()
	if err = os.MkdirAll(s.root, 0700); err != nil {
		return err
	}
	// Bound this library to 1 GiB; model files are immutable and saves of
	// identical content are idempotent. Do not consume the episode partition.
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return err
	}
	var used int64
	for _, entry := range entries {
		// Only one upload runs at a time, so a temporary file found here was
		// left by a transfer the agent did not outlive. The quota cannot see it.
		if strings.HasPrefix(entry.Name(), ".upload-") || strings.HasPrefix(entry.Name(), ".index-") {
			os.Remove(filepath.Join(s.root, entry.Name()))
			continue
		}
		if strings.HasSuffix(entry.Name(), ".wendysite") && entry.Name() != info.Id+".wendysite" {
			stat, e := entry.Info()
			if e != nil {
				return e
			}
			used += stat.Size()
		}
	}
	if used+info.Size > 1<<30 {
		return status.Error(codes.ResourceExhausted, "site model storage limit reached (1 GiB)")
	}
	temp, err := os.CreateTemp(s.root, ".upload-")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	hash := sha256.New()
	var total int64
	var prefix []byte
	for {
		frame, e := stream.Recv()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		if frame.Info != nil || len(frame.Data) == 0 || len(frame.Data) > siteModelChunkBytes || total+int64(len(frame.Data)) > info.Size {
			return status.Error(codes.InvalidArgument, "invalid model chunk")
		}
		if len(prefix) < 8 {
			need := 8 - len(prefix)
			if need > len(frame.Data) {
				need = len(frame.Data)
			}
			prefix = append(prefix, frame.Data[:need]...)
		}
		if _, err = temp.Write(frame.Data); err != nil {
			return err
		}
		hash.Write(frame.Data)
		total += int64(len(frame.Data))
	}
	if total != info.Size || hex.EncodeToString(hash.Sum(nil)) != info.Id || (string(prefix) != "WENDYST1" && string(prefix) != "WENDYST2") {
		return status.Error(codes.DataLoss, "model size, checksum or archive header did not match")
	}
	if err = temp.Sync(); err != nil {
		return err
	}
	if err = temp.Close(); err != nil {
		return err
	}
	if err = stream.Context().Err(); err != nil {
		return err
	}
	if err = os.Rename(temp.Name(), filepath.Join(s.root, info.Id+".wendysite")); err != nil {
		return err
	}
	metadata, _ := json.Marshal(info)
	index, err := os.CreateTemp(s.root, ".index-")
	if err != nil {
		return err
	}
	defer os.Remove(index.Name())
	if _, err = index.Write(metadata); err != nil {
		index.Close()
		return err
	}
	if err = index.Sync(); err != nil {
		index.Close()
		return err
	}
	if err = index.Close(); err != nil {
		return err
	}
	if err = os.Rename(index.Name(), filepath.Join(s.root, info.Id+".json")); err != nil {
		return err
	}
	return stream.SendAndClose(info)
}
func (s *SiteModelService) Download(req *pb.SiteModelDownloadRequest, stream grpc.ServerStreamingServer[pb.SiteModelChunk]) error {
	if err := authorizeSiteModels(stream.Context()); err != nil {
		return err
	}
	if !siteModelID.MatchString(req.Id) {
		return status.Error(codes.InvalidArgument, "invalid model ID")
	}
	path := filepath.Join(s.root, req.Id+".wendysite")
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return status.Error(codes.NotFound, "model not found")
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > SiteModelMaxBytes {
		return status.Error(codes.DataLoss, "invalid stored model")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	buf := make([]byte, siteModelChunkBytes)
	for {
		n, e := file.Read(buf)
		if n > 0 {
			if err = stream.Send(&pb.SiteModelChunk{Data: buf[:n]}); err != nil {
				return err
			}
		}
		if e == io.EOF {
			return nil
		}
		if e != nil {
			return e
		}
	}
}
