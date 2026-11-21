package services

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/data/thermal"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

type thermalMeasurement struct {
	Celsius                      float64
	Identity, Source, Provenance string
	X, Y                         uint32
}
type thermalReader interface {
	Next(context.Context) (thermalMeasurement, error)
	Close()
}

type thermalCameraReader struct {
	hub    *deviceHub
	id     int
	frames chan *videoFrame
	source string
}

func (r *thermalCameraReader) Close() { r.hub.unsubscribe(r.id) }
func (r *thermalCameraReader) Next(ctx context.Context) (thermalMeasurement, error) {
	select {
	case <-ctx.Done():
		return thermalMeasurement{}, ctx.Err()
	case f, ok := <-r.frames:
		if !ok {
			return thermalMeasurement{}, fmt.Errorf("thermal camera stream ended")
		}
		age := time.Since(time.Unix(0, int64(f.tsNs)))
		if f.tsNs == 0 || age < -time.Second || age > 3*time.Second {
			return thermalMeasurement{}, fmt.Errorf("stale thermal camera frame")
		}
		m := thermalMeasurement{Source: r.source}
		if summary := thermal.DecodeTemperatureSummary(f.data, f.rawFmt); summary != nil {
			m.Celsius, m.X, m.Y = summary.HotCelsius, summary.Hottest.X, summary.Hottest.Y
			m.Identity, m.Provenance = fmt.Sprint(summary.Identity), "HIK camera-reported Celsius summary"
			return m, nil
		}
		decoded, err := thermal.Decode(f.data, f.rawFmt)
		if err != nil {
			return m, err
		}
		if decoded.Temperature == nil {
			return m, fmt.Errorf("camera has no verified Celsius conversion")
		}
		m.Celsius = float64(decoded.Hottest.Raw)*decoded.Temperature.KelvinPerCount + decoded.Temperature.KelvinOffset - 273.15
		m.X, m.Y = decoded.Hottest.X, decoded.Hottest.Y
		m.Identity, m.Provenance = fmt.Sprint(f.tsNs), decoded.Temperature.Provenance
		return m, nil
	}
}

type mlxThermalReader struct {
	url    string
	client *http.Client
}

func (r *mlxThermalReader) Close() { r.client.CloseIdleConnections() }
func (r *mlxThermalReader) Next(ctx context.Context) (thermalMeasurement, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return thermalMeasurement{}, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return thermalMeasurement{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return thermalMeasurement{}, fmt.Errorf("thermal viewer HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if err != nil {
		return thermalMeasurement{}, err
	}
	if len(body) > 65536 {
		return thermalMeasurement{}, fmt.Errorf("thermal viewer response exceeds 64KiB")
	}
	return decodeMLXThermal(body)
}
func decodeMLXThermal(body []byte) (thermalMeasurement, error) {
	var e struct {
		Status string
		Age    *float64
		Frame  *struct {
			Width, Height int
			Pixels        []float64
			Seq, Number   uint64
			Timestamp     uint64 `json:"timestamp_us"`
		}
	}
	m := thermalMeasurement{Source: "app:dev.wendy.thermal-viewer/mlx90642", Provenance: "MLX90642 Celsius samples via USB thermal viewer"}
	if err := json.Unmarshal(body, &e); err != nil {
		return m, err
	}
	if e.Status != "live" || e.Age == nil || *e.Age < 0 || *e.Age >= 3 || e.Frame == nil || e.Frame.Width != 32 || e.Frame.Height != 24 || len(e.Frame.Pixels) != 768 || e.Frame.Timestamp == 0 {
		return m, fmt.Errorf("thermal viewer has no fresh 32x24 Celsius frame")
	}
	m.Celsius = -math.MaxFloat64
	for i, v := range e.Frame.Pixels {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < -273.15 || v > 1000 {
			return m, fmt.Errorf("invalid thermal viewer temperature")
		}
		if v > m.Celsius {
			m.Celsius, m.X, m.Y = v, uint32(i%32), uint32(i/32)
		}
	}
	m.Identity = fmt.Sprintf("%d/%d/%d", e.Frame.Number, e.Frame.Seq, e.Frame.Timestamp)
	return m, nil
}

func (j *campaignInferenceJob) openThermal(ctx context.Context) (thermalReader, error) {
	c := j.campaign.Thermal
	if c.MLX90642URL != "" {
		return &mlxThermalReader{url: c.MLX90642URL, client: &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error {
			return fmt.Errorf("thermal viewer redirects are not allowed")
		}}}, nil
	}
	video, ok := j.owner.service.video.(*VideoService)
	if !ok {
		return nil, fmt.Errorf("thermal camera runtime unavailable")
	}
	plan := j.campaign
	plan.Sources = []data.CampaignSource{{Camera: c.Camera}}
	sources, err := j.owner.service.manager.ResolveCampaignCameraSources(plan)
	if err != nil {
		return nil, err
	}
	if len(sources) != 1 {
		return nil, fmt.Errorf("thermal camera selector must resolve to exactly one camera")
	}
	source := sources[0].ID
	src, err := video.resolveSensorSource(source)
	if err != nil {
		return nil, err
	}
	if src.kind != sourceV4L2 {
		return nil, fmt.Errorf("thermal camera must provide local raw frames")
	}
	devID, _ := cameraDeviceID(source)
	hub, id, frames, err := video.joinHub(ctx, src.key, &pb.StreamVideoRequest{DeviceId: devID, Codec: pb.VideoCodec_VIDEO_CODEC_RAW})
	if err != nil {
		return nil, err
	}
	return &thermalCameraReader{hub: hub, id: id, frames: frames, source: source}, nil
}

func (j *campaignInferenceJob) runThermal(ctx context.Context) {
	var latch data.ThermalLatch
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	var reader thermalReader
	defer func() {
		if reader != nil {
			reader.Close()
		}
	}()
	var identity string
	// A first HIK summary may be cached. Every connection must prove advancement.
	var advanced bool
	lastAdvance := time.Now()
	for ctx.Err() == nil {
		var err error
		if reader == nil {
			reader, err = j.openThermal(ctx)
			identity = ""
			advanced = false
			lastAdvance = time.Now()
		}
		var m thermalMeasurement
		if err == nil {
			sampleCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			m, err = reader.Next(sampleCtx)
			cancel()
		}
		now := time.Now()
		if err != nil {
			latch.Observe(j.campaign.Thermal, now, 0, false)
			j.setState("error", err)
			if reader != nil {
				reader.Close()
				reader = nil
			}
		} else if m.Identity != identity {
			if identity != "" {
				advanced = true
			}
			identity = m.Identity
			lastAdvance = now
			if advanced {
				j.setState("running", nil)
				if latch.Observe(j.campaign.Thermal, now, m.Celsius, true) {
					if err := j.owner.service.triggerThermal(ctx, j.campaign, m); err != nil {
						j.setState("error", err)
					}
				}
			} else {
				j.setState("waiting", nil)
			}
		} else if now.Sub(lastAdvance) > 3*time.Second {
			latch.Observe(j.campaign.Thermal, now, 0, false)
			j.setState("error", fmt.Errorf("thermal source is repeating a stale reading"))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *DataService) triggerThermal(ctx context.Context, campaign data.Campaign, m thermalMeasurement) error {
	s.deploymentMu.Lock()
	defer s.deploymentMu.Unlock()
	current, err := s.manager.Campaign(campaign.Name)
	if err != nil {
		return err
	}
	if ctx.Err() != nil || current.State != data.CampaignStateArmed || current.Revision != campaign.Revision || current.Thermal == nil {
		return nil
	}
	record := data.ApplicationRecord{Version: 1, Type: "event", Name: current.Thermal.Event, Attributes: map[string]any{"campaign": current.Name, "source_id": m.Source, "max_celsius": m.Celsius, "x": m.X, "y": m.Y, "measurement_id": m.Identity, "provenance": m.Provenance, "above_celsius": current.Thermal.AboveCelsius, "for": current.Thermal.For}}
	if _, err = s.manager.RecordCampaignApplication(data.CampaignAppID(current.Name), record); err != nil {
		return err
	}
	if _, active := s.manager.ActiveSession(current.Name); active {
		return nil
	}
	_, err = s.triggerCampaign(ctx, current, "event:"+record.Name, "event:"+record.Name)
	return err
}
