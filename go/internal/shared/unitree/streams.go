package unitree

// Topic subscriptions and the front-camera video track. Wire format from
// unitree_webrtc_connect@e0abc5780761539eff89a382a79319e5cf6ad1f4
// webrtc_datachannel.py, msgs/pub_sub.py and webrtc_video.py.
import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"
)

const (
	maxSubscriptionTopics = 32
	maxBinaryMessage      = 8 << 20
	topicBuffer           = 256
	videoBuffer           = 64
)

var topicPattern = regexp.MustCompile(`^rt/[A-Za-z0-9_/]{1,120}$`)

// ValidTopic accepts robot topic names such as "rt/lf/lowstate".
func ValidTopic(topic string) bool { return topicPattern.MatchString(topic) }

// TopicMessage is one message as the robot sent it. Text messages carry the
// JSON envelope; binary messages (LiDAR) carry the whole framed buffer.
type TopicMessage struct {
	Topic    string
	Data     []byte
	Binary   bool
	Received time.Time
}

// VideoFrame is one H.264 access unit in Annex-B format.
type VideoFrame struct {
	AnnexB       []byte
	RTPTimestamp uint32
	Received     time.Time
}

type subscriber struct {
	topics   map[string]bool
	messages chan TopicMessage
	video    chan VideoFrame // nil unless video was requested
}

// Subscribe starts delivery of the given topics and, optionally, video. The
// robot is asked to send a topic, LiDAR or video only while at least one
// subscriber wants it. Slow readers lose messages rather than stalling the
// robot connection. Call cancel exactly once when done.
func (c *Connection) Subscribe(topics []string, video bool) (<-chan TopicMessage, <-chan VideoFrame, func(), error) {
	if len(topics) == 0 && !video {
		return nil, nil, nil, errors.New("subscribe to at least one topic or video")
	}
	if len(topics) > maxSubscriptionTopics {
		return nil, nil, nil, errors.New("too many Go2 topics")
	}
	s := &subscriber{topics: make(map[string]bool, len(topics)), messages: make(chan TopicMessage, topicBuffer)}
	for _, topic := range topics {
		if !ValidTopic(topic) {
			return nil, nil, nil, errors.New("invalid Go2 topic name")
		}
		s.topics[topic] = true
	}
	if video {
		s.video = make(chan VideoFrame, videoBuffer)
	}
	if !c.Connected() {
		return nil, nil, nil, errors.New("Go2 is not connected")
	}

	c.subMu.Lock()
	if c.subscribers == nil {
		c.subscribers = make(map[*subscriber]struct{})
		c.topicRefs = make(map[string]int)
	}
	c.subscribers[s] = struct{}{}
	var err error
	for topic := range s.topics {
		c.topicRefs[topic]++
		if c.topicRefs[topic] == 1 {
			err = errors.Join(err, c.sendTopic("subscribe", topic, nil))
		}
		if isLidarTopic(topic) {
			c.lidarRefs++
			if c.lidarRefs == 1 {
				err = errors.Join(err, c.sendLidar(true))
			}
		}
	}
	if video {
		c.videoRefs++
		if c.videoRefs == 1 {
			err = errors.Join(err, c.send("vid", "on"))
		}
		c.requestKeyframe()
	}
	c.subMu.Unlock()

	cancel := func() { c.unsubscribe(s) }
	if err != nil {
		cancel()
		return nil, nil, nil, errors.New("Go2 subscription request failed")
	}
	return s.messages, s.video, cancel, nil
}

func (c *Connection) unsubscribe(s *subscriber) {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	if _, ok := c.subscribers[s]; !ok {
		return
	}
	delete(c.subscribers, s)
	// Best effort: the connection may already be gone.
	for topic := range s.topics {
		c.topicRefs[topic]--
		if c.topicRefs[topic] == 0 {
			delete(c.topicRefs, topic)
			_ = c.sendTopic("unsubscribe", topic, nil)
		}
		if isLidarTopic(topic) {
			c.lidarRefs--
			if c.lidarRefs == 0 {
				_ = c.sendLidar(false)
			}
		}
	}
	if s.video != nil {
		c.videoRefs--
		if c.videoRefs == 0 {
			_ = c.send("vid", "off")
		}
	}
}

func isLidarTopic(topic string) bool { return strings.HasPrefix(topic, "rt/utlidar/") }

func (c *Connection) sendTopic(kind, topic string, data any) error {
	message := map[string]any{"type": kind, "topic": topic}
	if data != nil {
		message["data"] = data
	}
	raw, err := json.Marshal(message)
	if err != nil {
		return err
	}
	return c.channel.SendText(string(raw))
}

// sendLidar turns the LiDAR stream on or off. The robot throttles large
// messages unless traffic saving is disabled while LiDAR is wanted.
func (c *Connection) sendLidar(on bool) error {
	state := map[bool]string{true: "on", false: "off"}[on]
	return errors.Join(
		c.send("rtc_inner_req", map[string]string{"req_type": "disable_traffic_saving", "instruction": state}),
		c.sendTopic("msg", "rt/utlidar/switch", state),
	)
}

func (c *Connection) dispatchTopic(topic string, data []byte, isBinary bool) {
	message := TopicMessage{Topic: topic, Data: data, Binary: isBinary, Received: time.Now()}
	c.subMu.Lock()
	defer c.subMu.Unlock()
	for s := range c.subscribers {
		if !s.topics[topic] {
			continue
		}
		select {
		case s.messages <- message:
		default: // drop for a slow reader
		}
	}
}

func (c *Connection) dispatchVideo(frame VideoFrame) {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	for s := range c.subscribers {
		if s.video == nil {
			continue
		}
		select {
		case s.video <- frame:
		default:
		}
	}
}

// binaryTopic reads the topic from a framed binary message. LiDAR messages
// start with uint16 2, uint16 0, a uint32 JSON length and 4 reserved bytes;
// other binary messages start with a uint16 JSON length and 2 reserved bytes.
func binaryTopic(buf []byte) (string, bool) {
	if len(buf) < 4 {
		return "", false
	}
	var header []byte
	if binary.LittleEndian.Uint16(buf) == 2 && binary.LittleEndian.Uint16(buf[2:]) == 0 {
		if len(buf) < 12 {
			return "", false
		}
		n := binary.LittleEndian.Uint32(buf[4:])
		if uint64(n) > uint64(len(buf)-12) {
			return "", false
		}
		header = buf[12 : 12+n]
	} else {
		n := int(binary.LittleEndian.Uint16(buf))
		if n > len(buf)-4 {
			return "", false
		}
		header = buf[4 : 4+n]
	}
	var envelope struct {
		Topic string `json:"topic"`
	}
	if json.Unmarshal(header, &envelope) != nil || !ValidTopic(envelope.Topic) {
		return "", false
	}
	return envelope.Topic, true
}

func (c *Connection) onTrack(track *webrtc.TrackRemote) {
	if track.Kind() != webrtc.RTPCodecTypeVideo {
		for { // audio is unused; keep reading so buffers drain
			if _, _, err := track.ReadRTP(); err != nil {
				return
			}
		}
	}
	c.videoSSRC.Store(uint32(track.SSRC()))
	c.requestKeyframe()
	builder := samplebuilder.New(128, &codecs.H264Packet{}, 90000)
	for {
		packet, _, err := track.ReadRTP()
		if err != nil {
			return
		}
		builder.Push(packet)
		for sample := builder.Pop(); sample != nil; sample = builder.Pop() {
			c.dispatchVideo(VideoFrame{AnnexB: sample.Data, RTPTimestamp: sample.PacketTimestamp, Received: time.Now()})
		}
	}
}

// requestKeyframe asks the robot for an IDR frame so a new viewer starts
// quickly. Harmless before the video track exists.
func (c *Connection) requestKeyframe() {
	if ssrc := c.videoSSRC.Load(); ssrc != 0 {
		_ = c.peer.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: ssrc}})
	}
}
