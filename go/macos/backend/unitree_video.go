package backend

import (
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/unitree"
)

type unitreeVideoSource interface {
	Subscribe([]string, bool) (<-chan unitree.TopicMessage, <-chan unitree.VideoFrame, func(), error)
}

func (c *UnitreeController) StartVideo(handle uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if handle == 0 || handle != c.handle || c.link == nil || !c.link.Connected() {
		return errors.New("Go2 camera is disconnected")
	}
	if c.videoCancel != nil {
		return nil
	}
	source, ok := c.link.(unitreeVideoSource)
	if !ok {
		return errors.New("Go2 camera is unavailable")
	}
	_, frames, cancel, err := source.Subscribe(nil, true)
	if err != nil {
		return err
	}
	c.videoFrames, c.videoCancel = frames, cancel
	return nil
}

// Never hold the motion lock while waiting for a camera frame.
func (c *UnitreeController) NextVideo(handle uint64) ([]byte, error) {
	c.mu.Lock()
	if handle == 0 || handle != c.handle || c.link == nil || !c.link.Connected() || c.videoFrames == nil {
		c.mu.Unlock()
		return nil, errors.New("Go2 camera is disconnected")
	}
	frames := c.videoFrames
	c.mu.Unlock()
	select {
	case frame, ok := <-frames:
		if !ok {
			return nil, errors.New("Go2 camera stopped")
		}
		if len(frame.AnnexB) > 8<<20 {
			return nil, errors.New("Go2 camera frame is too large")
		}
		return frame.AnnexB, nil
	case <-time.After(250 * time.Millisecond):
		return nil, nil
	}
}

func (c *UnitreeController) stopVideoLocked() {
	if c.videoCancel != nil {
		c.videoCancel()
	}
	c.videoCancel, c.videoFrames = nil, nil
}

func (c *UnitreeController) StopVideo(handle uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if handle == c.handle {
		c.stopVideoLocked()
	}
}
