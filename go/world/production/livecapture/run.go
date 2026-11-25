// Package livecapture publishes stepped raw multimodal observations through
// the existing ordinary app publisher and evidence API.
package livecapture

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/liveapp"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
)

const MaxResponseBytes = 8 << 20

type Capture struct {
	Capture sensorrecord.Capture `json:"capture"`
	Media   []byte               `json:"media"`
}

// Decode preserves original sample bytes and validates optional PNG/WAV media.
// Gas and LiDAR are inline observations and require no evidence upload.
func Decode(raw []byte, c Config) (Capture, error) {
	var out Capture
	if authority.DecodeStrictLimit(raw, &out, MaxResponseBytes) != nil || sensorrecord.ValidateMedia(out.Capture, out.Media) != nil {
		return out, liveapp.ErrCapture
	}
	s, err := out.Capture.Sample()
	if err != nil || s.Kind != sensorKind(c.SensorID) || s.SensorID != c.SensorID || s.RunID != c.RunID || s.SiteID != c.Site || s.Frame != c.Frame || out.Capture.SimulationAnchorUnixNS != c.SimulationAnchorUnixNS {
		return out, liveapp.ErrCapture
	}
	return out, nil
}

func Run(ctx context.Context, c Config, api appv1.WorldAppServiceClient, emit func(liveapp.Receipt) error) error {
	c, err := normalized(c)
	if err != nil {
		return err
	}
	u, err := url.Parse(c.SceneURL)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || sensorKind(c.SensorID) == "" || c.DurationMS < 1 || c.DurationMS > 120000 || c.MaxSamples < 1 || c.MaxSamples > 64 || c.PollIntervalMS < 50 || c.PollIntervalMS > 10000 || c.RequestTimeoutMS < 10 || c.RequestTimeoutMS > 5000 || ctx == nil || emit == nil {
		return liveapp.ErrConfig
	}
	token, err := os.ReadFile(c.ReadTokenFile)
	if err != nil || len(token) > 257 {
		return liveapp.ErrConfig
	}
	token = bytes.TrimSuffix(token, []byte("\n"))
	if len(token) < 32 || strings.ContainsAny(string(token), "\r\n \t") {
		return liveapp.ErrConfig
	}
	publisher, err := liveapp.NewPublisher(api, liveapp.PublisherConfig{RequestTimeout: time.Duration(c.RequestTimeoutMS) * time.Millisecond})
	if err != nil {
		return err
	}
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Duration(c.RequestTimeoutMS) * time.Millisecond, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("source redirects refused") }}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.DurationMS)*time.Millisecond)
	defer cancel()
	var sequence uint64
	var previous []byte
	for count := 0; count < c.MaxSamples; {
		req, e := http.NewRequestWithContext(ctx, http.MethodGet, c.SceneURL+"/v1/captures/"+c.SensorID, nil)
		if e != nil {
			return e
		}
		req.Header.Set("Authorization", "Bearer "+string(token))
		res, e := client.Do(req)
		if e != nil {
			return e
		}
		raw, e := io.ReadAll(io.LimitReader(res.Body, MaxResponseBytes+1))
		res.Body.Close()
		if e != nil || len(raw) > MaxResponseBytes || (res.StatusCode != 200 && res.StatusCode != 204) {
			return liveapp.ErrCapture
		}
		if res.StatusCode == 200 {
			capture, e := Decode(raw, c)
			if e != nil {
				return e
			}
			sample, _ := capture.Capture.Sample()
			if sample.Sequence < sequence || sample.Sequence == sequence && !bytes.Equal(previous, raw) {
				return liveapp.ErrCapture
			}
			if sample.Sequence > sequence {
				request, e := sensorrecord.Publication(capture.Capture, time.Duration(c.TTLMS)*time.Millisecond)
				if e != nil {
					return e
				}
				receipt, e := publisher.Submit(ctx, request, capture.Media)
				if e != nil {
					return e
				}
				if e = emit(receipt); e != nil {
					return e
				}
				sequence, previous = sample.Sequence, bytes.Clone(raw)
				count++
			}
		}
		if count == c.MaxSamples {
			return nil
		}
		timer := time.NewTimer(time.Duration(c.PollIntervalMS) * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}
