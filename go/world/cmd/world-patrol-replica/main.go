// world-patrol-replica publishes local simulated patrol results and provides a
// read-only native debrief bridge. It has no task or motion operation.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/patrolreplica"
)

type config struct {
	Public         patrolreplica.Config `json:"public"`
	Client         client.Config        `json:"client"`
	EvidenceClient *client.Config       `json:"evidence_client,omitempty"`
	Reader         string               `json:"reader,omitempty"`
	StatusURL      string               `json:"status_url,omitempty"`
	CaptureDir     string               `json:"capture_dir,omitempty"`
}

func load(path string, out any) error {
	b, e := patrolreplica.ReadFile(path, 2<<20)
	if e != nil {
		return e
	}
	return patrolreplica.Decode(b, out, 2<<20)
}
func status(ctx context.Context, address string) ([]byte, error) {
	u, e := url.Parse(address)
	if e != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Path != "/status" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("executor status must be an exact loopback /status URL")
	}
	req, e := http.NewRequestWithContext(ctx, "GET", address, nil)
	if e != nil {
		return nil, e
	}
	h := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("status redirect denied") }}
	res, e := h.Do(req)
	if e != nil {
		return nil, e
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("status HTTP %d", res.StatusCode)
	}
	b, e := io.ReadAll(io.LimitReader(res.Body, 1<<20+1))
	if len(b) > 1<<20 {
		return nil, errors.New("status too large")
	}
	return b, e
}
func run(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return errors.New("use setup, publish, or read")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	path := fs.String("config", "", "private JSON config")
	out := fs.String("out", "", "private journal/view directory")
	once := fs.Bool("once", false, "one bounded poll")
	statusURL := fs.String("status-url", "", "loopback executor status (setup only)")
	captures := fs.String("captures", "", "executor captures directory (setup only)")
	base := fs.Int("base-port", 19720, "six simulation loopback ports")
	if e := fs.Parse(args[1:]); e != nil {
		return e
	}
	if *out == "" || !filepath.IsAbs(*out) {
		return errors.New("absolute output directory required")
	}
	if args[0] == "setup" {
		return setup(ctx, *out, *statusURL, *captures, *base)
	}
	var cfg config
	if e := load(*path, &cfg); e != nil {
		return e
	}
	if e := cfg.Public.Validate(); e != nil {
		return e
	}
	node, e := client.New(cfg.Client)
	if e != nil {
		return e
	}
	if e = os.MkdirAll(*out, 0700); e != nil {
		return e
	}
	var poll func() error
	if args[0] == "publish" {
		p, e := patrolreplica.OpenPublisher(ctx, cfg.Public, cfg.Client, patrolreplica.NodeBackend{Client: node}, filepath.Join(*out, "journal.json"))
		if e != nil {
			return e
		}
		defer p.Close()
		poll = func() error {
			b, e := status(ctx, cfg.StatusURL)
			if e != nil {
				return e
			}
			now := time.Now()
			s, v, e := patrolreplica.DecodeSnapshot(b, cfg.Public, now)
			if e != nil {
				return e
			}
			liveErr := p.Progress(ctx, s, v, now)
			e = p.Collect(ctx, cfg.CaptureDir, s, v, now)
			receipt := map[string]any{"schema": "wendy.nav.patrol.publisher.v1", "updated_unix_ms": now.UnixMilli(), "reports": p.Count(), "capture_limit_reached": p.Count() >= patrolreplica.MaxReports-1, "mission_id": cfg.Public.MissionID, "run_id": cfg.Public.RunID, "live_error": safe(liveErr), "report_error": safe(e)}
			if we := patrolreplica.AtomicJSON(filepath.Join(*out, "status.json"), receipt); we != nil {
				return we
			}
			if e != nil {
				return e
			}
			return liveErr
		}
	} else if args[0] == "read" {
		if cfg.Reader == "" || cfg.EvidenceClient == nil {
			return errors.New("reader and capture-node credentials required")
		}
		evidence, e := client.New(*cfg.EvidenceClient)
		if e != nil {
			return e
		}
		if cfg.Client.Grant.Grant.Tenant != cfg.Public.Tenant || cfg.Client.Grant.Grant.Site != cfg.Public.Site || cfg.EvidenceClient.Grant.Grant.Tenant != cfg.Public.Tenant || cfg.EvidenceClient.Grant.Grant.Site != cfg.Public.Site {
			return errors.New("reader scope mismatch")
		}
		for _, name := range []string{"commands", "receipts", "media"} {
			if e = os.MkdirAll(filepath.Join(*out, name), 0700); e != nil {
				return e
			}
		}
		history, e := patrolreplica.OpenHistory(filepath.Join(*out, "history.json"), cfg.Public, patrolreplica.JSONHash(cfg.Client.Grant)+"/"+cfg.Client.Audience)
		if e != nil {
			return e
		}
		defer history.Close()
		view := patrolreplica.View{Schema: "wendy.nav.patrol.debrief-view.v1", Config: cfg.Public, Reader: cfg.Reader, Reports: []patrolreplica.Report{}}
		poll = func() error {
			snapshot, e := node.Query(ctx)
			now := time.Now()
			view.UpdatedMS = now.UnixMilli()
			view.Connected = e == nil
			view.Error = safe(e)
			if e == nil {
				view.Live, view.Reports, view.Rejected = patrolreplica.Select(cfg.Public, snapshot.Components, now)
				accepted, historyError := history.Admit(cfg.Public, view.Reports)
				if historyError != nil && !errors.Is(historyError, patrolreplica.ErrHistoryConflict) {
					return historyError
				}
				view.Reports = accepted
				if historyError != nil {
					view.Error = historyError.Error()
					view.Rejected++
				}
			} else {
				view.Live = nil
			}
			if now.UnixMilli() >= cfg.Public.RetainUntilMS {
				view.Reports = []patrolreplica.Report{}
				view.Live = nil
			}
			if e = patrolreplica.AtomicJSON(filepath.Join(*out, "state.json"), view); e != nil {
				return e
			}
			return commands(ctx, *out, cfg.Public, evidence, view)
		}
	} else {
		return errors.New("unknown role")
	}
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	for {
		e := poll()
		if e != nil {
			fmt.Fprintln(os.Stderr, safe(e))
		}
		if *once {
			return e
		}
		if time.Now().UnixMilli() >= cfg.Public.RetainUntilMS {
			return errors.New("fixed debrief retention ended")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
	}
}
func safe(e error) string {
	if e == nil {
		return ""
	}
	s := e.Error()
	if len(s) > 240 {
		s = s[:240]
	}
	return s
}
func commands(ctx context.Context, dir string, c patrolreplica.Config, evidence *client.Client, view patrolreplica.View) error {
	entries, e := os.ReadDir(filepath.Join(dir, "commands"))
	if e != nil {
		return e
	}
	if len(entries) > 256 {
		return errors.New("native command bound exceeded")
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		receipt := filepath.Join(dir, "receipts", entry.Name())
		if _, e = os.Lstat(receipt); e == nil {
			continue
		}
		var command struct {
			Schema   string `json:"schema"`
			RunID    string `json:"run_id"`
			Action   string `json:"action"`
			ReportID string `json:"report_id"`
			Digest   string `json:"digest"`
		}
		e = load(filepath.Join(dir, "commands", entry.Name()), &command)
		result := map[string]any{"accepted": false, "action": "fetch", "report_id": command.ReportID, "digest": command.Digest}
		if e == nil && (command.Schema != "wendy.nav.patrol.debrief-command.v1" || command.RunID != c.RunID || command.Action != "fetch") {
			e = errors.New("unsupported read-only debrief command")
		}
		var report *patrolreplica.Report
		if e == nil {
			for _, r := range view.Reports {
				if r.ID == command.ReportID {
					copy := r
					report = &copy
					break
				}
			}
			if report == nil || !view.Connected {
				e = errors.New("report is not available from the current peer")
			}
		}
		if e == nil {
			var b []byte
			var ref patrolreplica.Evidence
			b, ref, e = patrolreplica.Fetch(ctx, c, evidence, *report, command.Digest)
			if e == nil {
				ext := ".json"
				if ref.ContentType == "image/png" {
					ext = ".png"
				} else if ref.ContentType == "image/jpeg" {
					ext = ".jpg"
				} else if ref.ContentType == "audio/wav" {
					ext = ".wav"
				}
				name := ref.SHA256 + ext
				e = os.WriteFile(filepath.Join(dir, "media", name), b, 0600)
				if e == nil {
					result["accepted"] = true
					result["path"] = "media/" + name
					result["size"] = ref.Size
					result["content_type"] = ref.ContentType
					result["verified_unix_ms"] = time.Now().UnixMilli()
					result["retain_until_unix_ms"] = c.RetainUntilMS
				}
			}
		}
		result["error"] = safe(e)
		if e = patrolreplica.AtomicJSON(receipt, result); e != nil {
			return e
		}
	}
	return nil
}
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if e := run(ctx, os.Args[1:]); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
