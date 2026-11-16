//go:build regression_chunk_stall

package containerd

import (
	"errors"
	"path/filepath"

	containerdclient "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/plugins/content/local"
	"go.uber.org/zap"
)

// NewChunkStallFixture supplies the real local content store, Bolt index and
// disk staging without contacting a daemon. Only regression builds expose it.
func NewChunkStallFixture(directory string) (*Client, func() error, error) {
	store, err := local.NewStore(filepath.Join(directory, "content"))
	if err != nil {
		return nil, nil, err
	}
	index, err := OpenChunkIndex(filepath.Join(directory, "index.db"), "", zap.NewNop())
	if err != nil {
		return nil, nil, err
	}
	client, err := containerdclient.New("", containerdclient.WithDefaultNamespace("default"),
		containerdclient.WithServices(containerdclient.WithContentStore(store)))
	if err != nil {
		index.Close()
		return nil, nil, err
	}
	return &Client{client: client, logger: zap.NewNop(), namespace: "default", chunkIndex: index,
			staging: newStaging(filepath.Join(directory, "staging"))}, func() error {
			return errors.Join(client.Close(), index.Close())
		}, nil
}
