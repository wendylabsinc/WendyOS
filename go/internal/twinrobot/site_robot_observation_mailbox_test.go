package twinrobot

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pierrec/lz4/v4"
	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func TestObservationMailboxCoalescesCloudsButPreservesPoseOrderAndTimestamps(t *testing.T) {
	mailbox := newObservationMailbox()
	var messages []*pb.UnitreeTopicMessage
	for i, topic := range []string{sitenav.PoseTopic, sitenav.CloudTopic, sitenav.JointTopic, sitenav.PoseTopic, sitenav.CloudTopic, sitenav.JointTopic, sitenav.PoseTopic} {
		message := &pb.UnitreeTopicMessage{Topic: topic, Data: []byte{byte(i)}, ReceivedUnixNanos: int64(100 - i)} // Source rollback must not be sorted away.
		messages = append(messages, message)
		if !mailbox.push(message) {
			t.Fatal("unexpected queue closure")
		}
	}
	for _, index := range []int{0, 3, 4, 5, 6} {
		message, terminal, _ := mailbox.pop(context.Background())
		if terminal || message != messages[index] || message.ReceivedUnixNanos != int64(100-index) {
			t.Fatalf("lost ingress order or rewrote timestamp at index %d", index)
		}
	}
	if mailbox.cloudDrops != 1 || mailbox.jointDrops != 1 || mailbox.posePeak != 3 {
		t.Fatal("incorrect bounded-queue counters")
	}
	mailbox.close("disconnected")
	if _, terminal, reason := mailbox.pop(context.Background()); !terminal || reason != "disconnected" {
		t.Fatal("closure not reported")
	}
}

func TestObservationMailboxOverflowAndDisconnectDiscardPendingPackets(t *testing.T) {
	for _, cause := range []string{"pose count", "pose bytes", "packet bytes", "disconnect"} {
		t.Run(cause, func(t *testing.T) {
			mailbox := newObservationMailbox()
			if !mailbox.push(&pb.UnitreeTopicMessage{Topic: sitenav.CloudTopic}) {
				t.Fatal("initial cloud rejected")
			}
			switch cause {
			case "pose count":
				for range maxQueuedObservationPoses {
					if !mailbox.push(&pb.UnitreeTopicMessage{Topic: sitenav.PoseTopic}) {
						t.Fatal("early queue overflow")
					}
				}
				if mailbox.push(&pb.UnitreeTopicMessage{Topic: sitenav.PoseTopic}) {
					t.Fatal("unbounded pose queue")
				}
			case "pose bytes":
				if mailbox.push(&pb.UnitreeTopicMessage{Topic: sitenav.PoseTopic, Data: make([]byte, maxQueuedPoseBytes+1)}) {
					t.Fatal("unbounded queued bytes")
				}
			case "packet bytes":
				if mailbox.push(&pb.UnitreeTopicMessage{Topic: sitenav.CloudTopic, Data: make([]byte, maxObservationPacketBytes+1)}) {
					t.Fatal("oversized cloud retained")
				}
			case "disconnect":
				mailbox.close("stream ended")
			}
			if packet, terminal, reason := mailbox.pop(context.Background()); !terminal || reason == "" || packet != nil {
				t.Fatal("queued cloud survived terminal stream failure")
			}
			if mailbox.push(&pb.UnitreeTopicMessage{Topic: sitenav.PoseTopic}) {
				t.Fatal("closed queue revived")
			}
		})
	}
}

func TestObservationPoseQueueOverflowStopsController(t *testing.T) {
	r, commands := readySiteRobot()
	r.ctx, r.cancel = context.WithCancel(context.Background())
	defer r.cancel()
	r.moving, r.lookState, r.lookTurn = true, 1, &siteLookTurn{}
	mailbox := newObservationMailbox()
	for range maxQueuedObservationPoses + 1 {
		mailbox.push(&pb.UnitreeTopicMessage{Topic: sitenav.PoseTopic})
	}
	r.processObservationMailbox(mailbox)
	status := r.Status()
	if !status.Disconnected || status.Moving || !strings.Contains(status.ReadinessMessage, "bounded queue") || len(commands.commands) != 1 || commands.commands[0].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
		t.Fatalf("queue overflow did not stop and disconnect: %+v", status)
	}
}

type mailboxTestStream struct {
	ctx                     context.Context
	messages                []*pb.UnitreeTopicMessage
	index                   int
	drained                 chan struct{}
	finish                  chan struct{}
	drainedOnce, finishOnce sync.Once
}

func (s *mailboxTestStream) Recv() (*pb.UnitreeStreamMessage, error) {
	if s.index < len(s.messages) {
		message := s.messages[s.index]
		s.index++
		return &pb.UnitreeStreamMessage{Kind: &pb.UnitreeStreamMessage_Topic{Topic: message}}, nil
	}
	s.drainedOnce.Do(func() { close(s.drained) })
	select {
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	case <-s.finish:
		return nil, io.EOF
	}
}
func (s *mailboxTestStream) end() { s.finishOnce.Do(func() { close(s.finish) }) }

func mailboxPose(t *testing.T, stamp time.Time, x float64) *pb.UnitreeTopicMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"topic": sitenav.PoseTopic, "data": map[string]any{
		"position": []float64{x, 0, .3}, "imu_state": map[string]any{"quaternion": []float64{1, 0, 0, 0}},
		"stamp": map[string]any{"sec": stamp.Unix(), "nanosec": stamp.Nanosecond()},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return &pb.UnitreeTopicMessage{Topic: sitenav.PoseTopic, Data: raw, ReceivedUnixNanos: stamp.UnixNano()}
}

func mailboxCloud(t *testing.T, stamp time.Time) *pb.UnitreeTopicMessage {
	t.Helper()
	raw := make([]byte, 2048)
	for i := range raw {
		raw[i] = 255
	}
	compressed := make([]byte, lz4.CompressBlockBound(len(raw)))
	n, err := lz4.CompressBlock(raw, compressed, nil)
	if err != nil || n == 0 {
		t.Fatal("compressing floor fixture", err)
	}
	header, err := json.Marshal(map[string]any{"topic": sitenav.CloudTopic, "data": map[string]any{"src_size": len(raw), "origin": []float64{-2, -2, 0}, "resolution": .05, "frame_id": "odom", "stamp": float64(stamp.UnixNano()) / 1e9}})
	if err != nil {
		t.Fatal(err)
	}
	packet := make([]byte, 12)
	binary.LittleEndian.PutUint16(packet, 2)
	binary.LittleEndian.PutUint32(packet[4:], uint32(len(header)))
	packet = append(append(packet, header...), compressed[:n]...)
	return &pb.UnitreeTopicMessage{Topic: sitenav.CloudTopic, Data: packet, Binary: true, ReceivedUnixNanos: stamp.UnixNano()}
}

func TestObservationIngressDrainsWhileConsumerBlockedAndKeepsNewestFreshData(t *testing.T) {
	r, commands := readySiteRobot()
	r.ctx, r.cancel = context.WithCancel(context.Background())
	defer r.cancel()
	r.observed.Pose = sitenav.Pose{}
	now := time.Now()
	r.lastObservationLog, r.lastNavigationLog = now, now
	stream := &mailboxTestStream{ctx: r.ctx, drained: make(chan struct{}), finish: make(chan struct{})}
	// An expired packet remains expired. Newer poses retain their original
	// timestamps while superseded clouds can be dropped during a slow decode.
	stream.messages = append(stream.messages, mailboxPose(t, now.Add(-time.Second), 0))
	for i := 0; i < 20; i++ {
		stamp := now.Add(time.Duration(-400+i*20) * time.Millisecond)
		stream.messages = append(stream.messages, mailboxPose(t, stamp, 0), mailboxCloud(t, stamp.Add(time.Millisecond)))
	}
	poseStamp, cloudStamp := now.Add(-2*time.Millisecond), now.Add(-time.Millisecond)
	stream.messages = append(stream.messages, mailboxPose(t, poseStamp, 0), mailboxCloud(t, cloudStamp))
	done := make(chan struct{})
	r.mu.Lock() // Simulate the controller lock being held by expensive cloud work.
	go func() { r.receiveObservations(stream); close(done) }()
	select {
	case <-stream.drained:
	case <-time.After(time.Second):
		r.mu.Unlock()
		stream.end()
		t.Fatal("gRPC ingress waited on the slow controller consumer")
	}
	r.mu.Unlock()
	defer func() { stream.end(); <-done }()
	deadline := time.Now().Add(time.Second)
	for {
		status := r.Status()
		if status.MeasurementsReady && status.CoalescedCloudObservations >= 20 {
			r.mu.Lock()
			pose, cloud := r.observed.Pose, r.observed.CloudAt
			r.mu.Unlock()
			if !pose.Received.Equal(poseStamp) || !pose.Captured.Equal(poseStamp) || math.Abs(cloud.Sub(cloudStamp).Seconds()) > 0.000001 {
				t.Fatal("freshness was renewed by rewriting original timestamps")
			}
			if status.ObservationPoseQueuePeak > maxQueuedObservationPoses {
				t.Fatal("unbounded pose queue")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("newest measurements did not recover: %+v", status)
		}
		time.Sleep(time.Millisecond)
	}
	r.mu.Lock()
	if len(commands.commands) != 0 {
		r.mu.Unlock()
		t.Fatal("receiving observations issued motion")
	}
	r.moving, r.lookState, r.lookTurn = true, 1, &siteLookTurn{}
	r.mu.Unlock()
	stream.end()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("disconnect did not stop the worker")
	}
	status := r.Status()
	if !status.Disconnected || status.Moving || len(commands.commands) != 1 || commands.commands[0].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
		t.Fatalf("stream close failed to stop: %+v", status)
	}
}

func TestObservationPoseFIFOCannotHideDiscontinuity(t *testing.T) {
	r, commands := readySiteRobot()
	r.ctx, r.cancel = context.WithCancel(context.Background())
	defer r.cancel()
	r.observed.Pose = sitenav.Pose{}
	now := time.Now()
	r.lastObservationLog = now
	r.moving, r.lookState, r.lookTurn = true, 1, &siteLookTurn{}
	stream := &mailboxTestStream{ctx: r.ctx, drained: make(chan struct{}), finish: make(chan struct{}), messages: []*pb.UnitreeTopicMessage{
		mailboxPose(t, now.Add(-30*time.Millisecond), 0), mailboxPose(t, now.Add(-20*time.Millisecond), 5), mailboxPose(t, now.Add(-10*time.Millisecond), 0),
	}}
	done := make(chan struct{})
	r.mu.Lock()
	go func() { r.receiveObservations(stream); close(done) }()
	select {
	case <-stream.drained:
	case <-time.After(time.Second):
		r.mu.Unlock()
		stream.end()
		t.Fatal("ingress blocked")
	}
	r.mu.Unlock()
	defer func() { stream.end(); <-done }()
	deadline := time.Now().Add(time.Second)
	for {
		status := r.Status()
		if status.LookState == 3 {
			r.mu.Lock()
			epoch := r.poseEpoch
			r.mu.Unlock()
			if status.Moving || status.Aligned || epoch != 1 || !strings.Contains(status.LastMotionFailure, "pose jumped") {
				t.Fatalf("coalescing hid pose discontinuity: %+v epoch=%d", status, epoch)
			}
			if len(commands.commands) != 1 || commands.commands[0].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
				t.Fatal("discontinuity failed to stop")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("queued pose jump was not processed")
		}
		time.Sleep(time.Millisecond)
	}
}
