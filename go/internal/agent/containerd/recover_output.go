package containerd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	containerdclient "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/errdefs"
	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/services"
)

type recoveredTaskOutput struct {
	pid    uint32
	io     cio.IO
	cancel context.CancelFunc
	once   sync.Once
}

func (r *recoveredTaskOutput) close() {
	r.once.Do(func() {
		r.cancel()
		if r.io != nil {
			r.io.Cancel()
			_ = r.io.Close()
		}
	})
}

func (c *Client) trackTaskOutput(name string, pid uint32) {
	c.outputMu.Lock()
	defer c.outputMu.Unlock()
	if c.outputPIDs == nil {
		c.outputPIDs = make(map[string]uint32)
	}
	c.outputPIDs[name] = pid
}

func (c *Client) forgetTaskOutput(name string, pid uint32) {
	c.outputMu.Lock()
	defer c.outputMu.Unlock()
	if c.outputPIDs[name] == pid {
		delete(c.outputPIDs, name)
	}
}

func (c *Client) closeRecoveredTaskOutput() {
	c.outputMu.Lock()
	c.outputClosed = true
	var attached []*recoveredTaskOutput
	for _, r := range c.recoveredOutput {
		attached = append(attached, r)
	}
	c.outputMu.Unlock()
	for _, r := range attached {
		r.close()
	}
}

type recoveredOutputWriter func([]byte)

func (w recoveredOutputWriter) Write(p []byte) (int, error) {
	// cio reuses its copy buffer; a publisher may retain a chunk.
	w(append([]byte(nil), p...))
	return len(p), nil
}

// preserveTaskFIFOs keeps attachment cleanup from unlinking a surviving task's
// shim FIFOs. The original FIFO set is retired only after confirmed task exit.
func preserveTaskFIFOs(fifos *cio.FIFOSet) *cio.FIFOSet {
	if fifos == nil {
		return nil
	}
	return cio.NewFIFOSet(fifos.Config, nil)
}

// RecoverRunningTaskOutput attaches one reader to every inherited Wendy task.
// Normal starts already own their output; repeated boot reconciliation must
// never compete with that reader. The per-container lifecycle lock serializes
// attachment with start/stop/delete, while the PID guards late exit callbacks.
func (c *Client) RecoverRunningTaskOutput(ctx context.Context, publish func(string, services.ContainerOutput)) error {
	ctx = c.withNamespace(ctx)
	ctrs, err := c.client.Containers(ctx, fmt.Sprintf("labels.%q", labelKeyAppVersion))
	if err != nil {
		return err
	}
	if publish == nil {
		publish = func(string, services.ContainerOutput) {}
	}
	var failures []error
	for _, ctr := range ctrs {
		if err := c.recoverTaskOutput(ctx, ctr, publish); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", ctr.ID(), err))
		}
	}
	return errors.Join(failures...)
}

func (c *Client) recoverTaskOutput(ctx context.Context, ctr containerdclient.Container, publish func(string, services.ContainerOutput)) error {
	name := ctr.ID()
	unlock := c.lockNetworkOperation(name)
	defer unlock()
	task, err := ctr.Task(ctx, nil)
	if errdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	status, err := task.Status(ctx)
	if err != nil || status.Status != containerdclient.Running || task.Pid() == 0 {
		return err
	}
	pid := task.Pid()
	c.outputMu.Lock()
	tracked, closed := c.outputPIDs[name] == pid, c.outputClosed
	c.outputMu.Unlock()
	if tracked || closed {
		return nil
	}
	// Wait and IO outlive this boot RPC; Close detaches without killing the task.
	waitCtx, cancel := context.WithCancel(c.withNamespace(context.Background()))
	exitCh, err := task.Wait(waitCtx)
	if err != nil {
		cancel()
		return err
	}
	stdout := recoveredOutputWriter(func(p []byte) { publish(name, services.ContainerOutput{Stdout: p}) })
	stderr := recoveredOutputWriter(func(p []byte) { publish(name, services.ContainerOutput{Stderr: p}) })
	attach := cio.NewAttach(cio.WithStreams(nil, io.Writer(stdout), io.Writer(stderr)))
	var fifos *cio.FIFOSet
	attachedTask, err := ctr.Task(ctx, func(existing *cio.FIFOSet) (cio.IO, error) {
		fifos = existing
		return attach(preserveTaskFIFOs(existing))
	})
	if err != nil {
		cancel()
		return err
	}
	r := &recoveredTaskOutput{pid: pid, io: attachedTask.IO(), cancel: cancel}
	status, err = attachedTask.Status(ctx)
	if err != nil || status.Status != containerdclient.Running || attachedTask.Pid() != pid {
		r.close()
		return err
	}
	c.outputMu.Lock()
	if c.outputClosed {
		c.outputMu.Unlock()
		r.close()
		return nil
	}
	if c.outputPIDs == nil {
		c.outputPIDs = make(map[string]uint32)
	}
	if c.recoveredOutput == nil {
		c.recoveredOutput = make(map[string]*recoveredTaskOutput)
	}
	c.outputPIDs[name] = pid
	c.recoveredOutput[name] = r
	c.outputMu.Unlock()
	go func() {
		exit := <-exitCh
		code, exitedAt, exitErr := exit.Result()
		if exitErr == nil {
			// Deliver remaining output before retiring this task's FIFO paths.
			if r.io != nil {
				r.io.Wait()
			}
			_ = fifos.Close()
			recordCtx, recordCancel := context.WithTimeout(context.Background(), 5*time.Second)
			c.recordContainerExit(recordCtx, name, int32(code), classifyExit(code, taskOOMKilled(recordCtx, task)), exitedAt)
			recordCancel()
			publish(name, services.ContainerOutput{Done: true})
		}
		r.close()
		c.outputMu.Lock()
		if c.recoveredOutput[name] == r {
			delete(c.recoveredOutput, name)
		}
		if c.outputPIDs[name] == r.pid {
			delete(c.outputPIDs, name)
		}
		c.outputMu.Unlock()
	}()
	c.logger.Info("Reattached running task output", zap.String("container_id", name), zap.Uint32("pid", pid))
	return nil
}
