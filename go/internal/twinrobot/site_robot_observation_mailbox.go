package twinrobot

import (
	"context"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

const (
	maxQueuedObservationPoses = 64
	maxQueuedPoseBytes        = 1 << 20
	maxObservationPacketBytes = 8 << 20
)

type queuedObservation struct {
	sequence uint64
	message  *pb.UnitreeTopicMessage
}

// Cloud processing may take longer than the cloud publication interval. The
// ingress goroutine never waits for that work or the controller lock. Replace
// pending clouds/joints, but retain a bounded pose FIFO so a jump, tilt or clock
// reset cannot disappear behind a newer pose. Overflow fails closed.
type observationMailbox struct {
	mu                     sync.Mutex
	notify                 chan struct{}
	sequence               uint64
	poses                  []queuedObservation
	poseBytes              int
	cloud, joints          *queuedObservation
	cloudDrops, jointDrops uint64
	posePeak               int
	closed                 bool
	failure                string
}

func newObservationMailbox() *observationMailbox {
	return &observationMailbox{notify: make(chan struct{}, 1)}
}

func (m *observationMailbox) wakeLocked() {
	select {
	case m.notify <- struct{}{}:
	default:
	}
}

func (m *observationMailbox) closeLocked(reason string) {
	if m.closed {
		return
	}
	m.closed, m.failure = true, reason
	m.poses, m.cloud, m.joints = nil, nil, nil
	m.poseBytes = 0
	m.wakeLocked()
}

func (m *observationMailbox) close(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closeLocked(reason)
}

func (m *observationMailbox) push(message *pb.UnitreeTopicMessage) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return false
	}
	if message.Topic != sitenav.PoseTopic && message.Topic != sitenav.CloudTopic && message.Topic != sitenav.JointTopic {
		return true
	}
	if len(message.Data) > maxObservationPacketBytes {
		m.closeLocked("Go2 observation packet exceeded the bounded input size. Reconnect to receive telemetry.")
		return false
	}
	m.sequence++
	packet := queuedObservation{sequence: m.sequence, message: message}
	switch message.Topic {
	case sitenav.PoseTopic:
		if len(m.poses) >= maxQueuedObservationPoses || m.poseBytes+len(message.Data) > maxQueuedPoseBytes {
			m.closeLocked("Go2 odometry processing fell behind its bounded queue. Movement stopped; reconnect to receive telemetry.")
			return false
		}
		m.poses = append(m.poses, packet)
		m.poseBytes += len(message.Data)
		m.posePeak = max(m.posePeak, len(m.poses))
	case sitenav.CloudTopic:
		if m.cloud != nil {
			m.cloudDrops++
		}
		m.cloud = &packet
	case sitenav.JointTopic:
		if m.joints != nil {
			m.jointDrops++
		}
		m.joints = &packet
	}
	m.wakeLocked()
	return true
}

// Pop by ingress order, not sorted source timestamps: sorting would hide a
// source-clock rollback. Poses preceding the newest cloud remain ahead of it,
// preserving the existing cloud/pose pairing checks. Original timestamps and
// payloads are never rewritten, and stale queued poses still get rejected.
func (m *observationMailbox) pop(ctx context.Context) (*pb.UnitreeTopicMessage, bool, string) {
	for {
		if ctx.Err() != nil {
			return nil, true, ""
		}
		m.mu.Lock()
		if m.closed {
			reason := m.failure
			m.mu.Unlock()
			return nil, true, reason
		}
		var next *queuedObservation
		if len(m.poses) > 0 {
			next = &m.poses[0]
		}
		if m.cloud != nil && (next == nil || m.cloud.sequence < next.sequence) {
			next = m.cloud
		}
		if m.joints != nil && (next == nil || m.joints.sequence < next.sequence) {
			next = m.joints
		}
		if next != nil {
			message := next.message
			switch message.Topic {
			case sitenav.PoseTopic:
				m.poseBytes -= len(message.Data)
				m.poses[0] = queuedObservation{}
				m.poses = m.poses[1:]
			case sitenav.CloudTopic:
				m.cloud = nil
			case sitenav.JointTopic:
				m.joints = nil
			}
			m.mu.Unlock()
			return message, false, ""
		}
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, true, ""
		case <-m.notify:
		}
	}
}

func (r *SiteRobot) processObservationMailbox(mailbox *observationMailbox) {
	for {
		message, terminal, reason := mailbox.pop(r.ctx)
		waiting := time.Now()
		r.mu.Lock()
		mailbox.mu.Lock()
		// A stream failure wins over a packet removed before we acquired mu.
		if mailbox.closed {
			terminal, reason = true, mailbox.failure
		}
		r.coalescedCloudObservations = mailbox.cloudDrops
		r.coalescedJointObservations = mailbox.jointDrops
		r.observationPoseQueuePeak = mailbox.posePeak
		mailbox.mu.Unlock()
		if terminal {
			if reason != "" && r.ctx.Err() == nil {
				r.disconnectStreamLocked(reason)
			}
			r.mu.Unlock()
			return
		}
		r.lastObservationLockWaitMS = time.Since(waiting).Seconds() * 1000
		r.lastObservationTopic = message.Topic
		r.receiveObservationLocked(message.Topic, message.Data, message.Binary, time.Unix(0, message.ReceivedUnixNanos), time.Now())
		r.mu.Unlock()
	}
}
