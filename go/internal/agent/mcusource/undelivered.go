package mcusource

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
)

// deliveredToday is the limit behind every undelivered-channel notice: the
// supervisor mounts cameras and microphones, and has nowhere to send the rest.
const deliveredToday = "only cameras (MJPEG or H.264) and microphones (16-bit PCM) are delivered today"

// maxListedChannels bounds a notice. The source decides how many channels it
// offers and what they are called, so names are also escaped and cut short.
const maxListedChannels = 16

// undeliveredNotices remembers the notice last logged for each pairing key.
// The supervisor retries with backoff, so a notice is logged again only when
// it changes, and is forgotten when its source is unpaired.
type undeliveredNotices struct {
	mu   sync.Mutex
	last map[string]string
}

// changed records notice as the current one for key and reports whether it
// differs from the one before. An empty notice means there is nothing to say.
func (n *undeliveredNotices) changed(key, notice string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.last[key] == notice {
		return false
	}
	if n.last == nil {
		n.last = make(map[string]string)
	}
	n.last[key] = notice
	return true
}

// forget drops what key was told, so a source that is paired again is told again.
func (n *undeliveredNotices) forget(key string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.last, key)
}

// reportUndelivered explains why a paired source delivers less than it offers.
// cams and mics are the channels streamOnce selected to mount; nothing selects
// a generic sensor, or video or audio in another codec. A source with only
// such channels mounts nothing and is retried forever, so without this notice
// it looks paired while delivering nothing.
func (s *Supervisor) reportUndelivered(p SensorPairing, m *sensorlinkpb.SensorManifest, cams, mics []*sensorlinkpb.SensorDescriptor) {
	selected := make(map[*sensorlinkpb.SensorDescriptor]bool, len(cams)+len(mics))
	for _, d := range slices.Concat(cams, mics) {
		selected[d] = true
	}
	var ignored []*sensorlinkpb.SensorDescriptor
	var offered []string
	for _, d := range m.GetSensors() {
		offered = append(offered, d.GetName())
		// A channel the allowlist leaves out is the user's choice, not a limit
		// of the agent, so it is not reported as ignored.
		if selected[d] || (len(p.SensorAllowlist) > 0 && !contains(p.SensorAllowlist, d.GetName())) {
			continue
		}
		ignored = append(ignored, d)
	}
	// Sources reorder their manifest across restarts; that is not a change.
	sort.SliceStable(ignored, func(i, j int) bool { return ignored[i].GetChannelId() < ignored[j].GetChannelId() })
	sort.Strings(offered)
	descriptions := make([]string, len(ignored))
	for i, d := range ignored {
		descriptions[i] = describeChannel(d)
	}

	level, msg := zap.WarnLevel, ""
	var detail []zap.Field
	switch {
	case len(ignored) > 0:
		msg = "sensor source has no channel that can be delivered to apps; " + deliveredToday
		if len(selected) > 0 {
			level, msg = zap.InfoLevel, "sensor source has channels that are not delivered to apps; "+deliveredToday
		}
		detail = []zap.Field{zap.String("ignored_channels", boundedList(descriptions, "; "))}
	case len(selected) > 0:
		// Nothing the pairing asks for is left out.
	case len(offered) == 0:
		msg = "sensor source reports no sensor channels; nothing is delivered to apps"
	default:
		// Nothing is selected or ignored, so the allowlist left every channel out.
		msg = "sensor source has no channel named in this pairing's sensor allowlist; nothing is delivered to apps"
		detail = []zap.Field{zap.String("sensor_allowlist", quotedList(p.SensorAllowlist)), zap.String("offered_channels", quotedList(offered))}
	}

	notice := msg
	for _, field := range detail {
		notice += "\n" + field.String
	}
	if !s.undelivered.changed(p.Key(), notice) || msg == "" {
		return
	}
	fields := []zap.Field{zap.String("source", p.Key())}
	if p.Name != "" {
		fields = append(fields, zap.String("name", p.Name))
	}
	s.logger.Log(level, msg, append(fields, detail...)...)
}

// describeChannel names a channel and says what it carries, for a log line.
func describeChannel(d *sensorlinkpb.SensorDescriptor) string {
	what := "no format declared"
	switch format := d.GetFormat().(type) {
	case *sensorlinkpb.SensorDescriptor_Sensor:
		what = "sensor, no schema declared"
		if schema := format.Sensor.GetSchema(); schema != "" {
			what = fmt.Sprintf("sensor, schema %.64q", schema)
		}
	case *sensorlinkpb.SensorDescriptor_Video:
		what = "video, " + describeCodec(format.Video.GetCodec())
	case *sensorlinkpb.SensorDescriptor_Audio:
		what = "audio, " + describeCodec(format.Audio.GetCodec())
	}
	return fmt.Sprintf("%.64q (channel %d): %s", d.GetName(), d.GetChannelId(), what)
}

// describeCodec is only asked about codecs the supervisor does not mount.
func describeCodec[C interface {
	~int32
	String() string
}](codec C) string {
	if codec == 0 {
		return "no codec declared"
	}
	return "unsupported codec " + codec.String()
}

func quotedList(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = fmt.Sprintf("%.64q", name)
	}
	return boundedList(quoted, ", ")
}

// boundedList joins the first maxListedChannels entries and counts the rest.
func boundedList(entries []string, separator string) string {
	if extra := len(entries) - maxListedChannels; extra > 0 {
		entries = append(entries[:maxListedChannels:maxListedChannels], fmt.Sprintf("and %d more", extra))
	}
	return strings.Join(entries, separator)
}
