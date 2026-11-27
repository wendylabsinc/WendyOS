package twin

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot"
)

func TestActiveLocalizationProgressBoundsStationaryWait(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 44*time.Second)
	defer cancel()
	message := activeLocalizationProgress(ctx, "Only parallel surfaces match; floor coverage is incomplete.", 5, 0)
	for _, want := range []string{"Only parallel surfaces", "floor coverage", "5 turns, 0 viewpoint moves", "At most 60 seconds"} {
		if !strings.Contains(message, want) {
			t.Fatalf("progress lost %q: %s", want, message)
		}
	}
}

func TestActiveLocalizationDeadlinePreservesCause(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	job := &robotSession{searchMessage: "Only parallel surfaces match. LiDAR floor coverage is incomplete."}
	result := (&Server{}).concludeActiveLocalization(ctx, job, twinrobot.SiteFusionRequest{}, nil)
	if result.State != "failed" || !strings.Contains(result.Message, "five-minute limit") || !strings.Contains(result.Message, job.searchMessage) {
		t.Fatalf("deadline hid cause: %+v", result)
	}
	if message := activeLocalizationProgress(ctx, "Waiting", 5, 0); !strings.Contains(message, "At most 0 seconds") {
		t.Fatalf("expired search acquired more time: %s", message)
	}
}
