package containerd

import (
	"context"
	"fmt"
	"sync"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/errdefs"
)

const (
	ros2ImagePreparationTimeout = 15 * time.Minute
	ros2ImageShutdownTimeout    = 5 * time.Second
)

type ros2ImagePreparation struct {
	mu     sync.Mutex
	job    *ros2ImageJob
	closed bool
}

type ros2ImageJob struct {
	done   chan struct{}
	cancel context.CancelFunc
	image  containerd.Image
	err    error
}

// wait shares one pull/unpack between host and system inspectors. A caller can
// stop waiting without aborting extraction for the next request. Only image
// preparation outlives the RPC; container creation still uses its context.
func (p *ros2ImagePreparation) wait(ctx context.Context, prepare func(context.Context) (containerd.Image, error)) (containerd.Image, error) {
	p.mu.Lock()
	if err := ctx.Err(); err != nil {
		p.mu.Unlock()
		return nil, err
	}
	if p.closed {
		p.mu.Unlock()
		return nil, context.Canceled
	}
	job := p.job
	if job == nil {
		setupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ros2ImagePreparationTimeout)
		job = &ros2ImageJob{done: make(chan struct{}), cancel: cancel}
		p.job = job
		go func() {
			defer cancel()
			job.image, job.err = prepare(setupCtx)
			p.mu.Lock()
			p.job = nil
			close(job.done)
			p.mu.Unlock()
		}()
	}
	p.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-job.done:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return job.image, job.err
	}
}

// close stops accepting work and gives a canceled job time to finish cleanup.
// It reports whether the job exited before the shutdown wait expired.
func (p *ros2ImagePreparation) close() bool {
	p.mu.Lock()
	p.closed = true
	job := p.job
	if job != nil {
		job.cancel()
	}
	p.mu.Unlock()
	if job == nil {
		return true
	}
	select {
	case <-job.done:
		return true
	case <-time.After(ros2ImageShutdownTimeout):
		return false
	}
}

func (c *Client) prepareROS2InspectorImage(ctx context.Context) (containerd.Image, error) {
	image, err := c.client.GetImage(ctx, ros2HostImage)
	if errdefs.IsNotFound(err) {
		image, err = c.client.Pull(ctx, ros2HostImage, containerd.WithPullUnpack)
	}
	if err != nil {
		return nil, fmt.Errorf("preparing standalone ROS 2 inspector image %q (first use requires downloading the image): %w", ros2HostImage, err)
	}
	unpacked, err := c.imageUnpacked(ctx, image)
	if err != nil {
		return nil, fmt.Errorf("checking host inspector image: %w", err)
	}
	if !unpacked {
		if err := c.UnpackImage(ctx, image, nil); err != nil {
			return nil, fmt.Errorf("unpacking host inspector image: %w", err)
		}
	}
	return image, nil
}
