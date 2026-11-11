package backend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const siteTransferLimit = 256 << 20

// Local paths are supplied by the app's archive store, never by the device.
func (b *Backend) SiteModels(session uint64, action, path, name, id string) (result []*pb.SiteModelInfo, err error) {
	defer func() {
		if status.Code(err) == codes.Unimplemented {
			err = errors.New("This device needs an agent update that supports saved site models.")
		}
	}()
	// A transfer can take minutes. Hold the request lock only to choose the
	// connection, so the device's apps, logs and cameras stay usable meanwhile.
	// The transfer ends with that connection.
	b.mu.Lock()
	conn, err := b.connection(session)
	if err != nil {
		b.mu.Unlock()
		return nil, err
	}
	life := b.life
	if life == nil {
		life = context.Background()
	}
	ctx, cancel := context.WithTimeout(life, 10*time.Minute)
	client := pb.NewSiteModelServiceClient(conn.Conn)
	b.mu.Unlock()
	defer cancel()
	switch action {
	case "list":
		value, e := client.List(ctx, &pb.SiteModelListRequest{})
		if e != nil {
			return nil, e
		}
		return value.Models, nil
	case "save":
		file, e := os.Open(path)
		if e != nil {
			return nil, e
		}
		defer file.Close()
		stat, e := file.Stat()
		if e != nil {
			return nil, e
		}
		if !stat.Mode().IsRegular() || stat.Size() > siteTransferLimit {
			return nil, errors.New("Model archive is too large")
		}
		hash := sha256.New()
		if _, e = io.Copy(hash, file); e != nil {
			return nil, e
		}
		if _, e = file.Seek(0, 0); e != nil {
			return nil, e
		}
		info := &pb.SiteModelInfo{Id: hex.EncodeToString(hash.Sum(nil)), Name: name, Size: stat.Size()}
		stream, e := client.Upload(ctx)
		if e != nil {
			return nil, e
		}
		// A device refuses an upload by ending the stream. Sending then reports
		// only io.EOF; the device's reason arrives with the response.
		e = stream.Send(&pb.SiteModelUploadRequest{Info: info})
		buf := make([]byte, 256<<10)
		for e == nil {
			n, readErr := file.Read(buf)
			if n > 0 {
				e = stream.Send(&pb.SiteModelUploadRequest{Data: buf[:n]})
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				return nil, readErr
			}
		}
		if e != nil && e != io.EOF {
			return nil, e
		}
		saved, e := stream.CloseAndRecv()
		if e != nil {
			return nil, e
		}
		if saved.Id != info.Id || saved.Size != info.Size {
			return nil, errors.New("Device did not confirm the saved model")
		}
		return []*pb.SiteModelInfo{saved}, nil
	case "load":
		if len(id) != 64 {
			return nil, errors.New("Invalid model ID")
		}
		stream, e := client.Download(ctx, &pb.SiteModelDownloadRequest{Id: id})
		if e != nil {
			return nil, e
		}
		file, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return nil, e
		}
		complete := false
		defer func() {
			file.Close()
			if !complete {
				os.Remove(path)
			}
		}()
		hash := sha256.New()
		var total int64
		for {
			chunk, e := stream.Recv()
			if e == io.EOF {
				break
			}
			if e != nil {
				return nil, e
			}
			total += int64(len(chunk.Data))
			if total > siteTransferLimit {
				return nil, errors.New("Device model exceeds size limit")
			}
			if _, e = file.Write(chunk.Data); e != nil {
				return nil, e
			}
			hash.Write(chunk.Data)
		}
		if hex.EncodeToString(hash.Sum(nil)) != id {
			return nil, errors.New("Downloaded model checksum did not match")
		}
		if e = file.Sync(); e != nil {
			return nil, e
		}
		if e = file.Close(); e != nil {
			return nil, e
		}
		complete = true
		return []*pb.SiteModelInfo{{Id: id, Size: total}}, nil
	default:
		return nil, errors.New("Unknown model transfer action")
	}
}
