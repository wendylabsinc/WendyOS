package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// PreviewThermalFrames reads all four channels through the authenticated app
// tunnel. Callers cannot supply a URL, port, app identity or redirect target.
func (b *Backend) PreviewThermalFrames(id uint64) (json.RawMessage, error) {
	p := b.preview(id)
	if p == nil {
		return nil, errors.New("camera preview is closed")
	}
	address, err := b.PreviewOpenApp(id, "dev.wendy.thermal-viewer")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(p.ctx, 5*time.Second)
	defer cancel()
	return readPreviewThermal(ctx, strings.TrimRight(address, "/")+"/api/frames")
}

func readPreviewThermal(ctx context.Context, address string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	client := http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("thermal viewer returned HTTP %d", res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, 262145))
	if err != nil {
		return nil, err
	}
	if len(data) > 262144 {
		return nil, errors.New("thermal response exceeds 256 KiB")
	}
	if !json.Valid(data) {
		return nil, errors.New("thermal viewer returned invalid JSON")
	}
	return data, nil
}
