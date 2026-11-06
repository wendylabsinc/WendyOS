package sitenav

import (
	"math"
	"testing"
	"time"
)

func expiryCloud(y float64) SurfaceCloud {
	var cloud SurfaceCloud
	for i := 0; i < 40; i++ {
		cloud.Surfaces = append(cloud.Surfaces, Surface{Position: Vertex{float64(i) * .2, y, .5}})
	}
	return cloud
}

func TestContinuousRetriesExpireHistoricalBias(t *testing.T) {
	now := time.Now()
	cloud := expiryCloud(0)
	first := RegistrationFit{Pose: PoseCandidate{Position: Point{1, 2}}, Cost: .01, Valid: true}
	other := RegistrationFit{Pose: PoseCandidate{Position: Point{4, 5}, Rotation: math.Pi}, Cost: .011, Valid: true}
	tracker := PoseTracker{}
	tracker.Update(Pose{}, cloud, []RegistrationFit{first}, now)
	// Old evidence distinguishes these otherwise ambiguous current fits.
	// Frequent attempts must not renew that evidence's acquisition time.
	for second := 1; second < 30; second++ {
		result := tracker.Update(Pose{}, cloud, []RegistrationFit{first, other}, now.Add(time.Duration(second)*time.Second))
		if result.Selected < 0 {
			t.Fatalf("recent history was lost at %d seconds", second)
		}
	}
	result := tracker.Update(Pose{}, cloud, []RegistrationFit{first, other}, now.Add(30*time.Second))
	if result.Selected != -1 || result.Fits[0].Cost != first.Cost || result.Fits[1].Cost != other.Cost {
		t.Fatalf("expired history still resolved an ambiguous match: %+v", result)
	}
	// Replaying the accumulated map after expiry still cannot corroborate it.
	for second := 31; second < 100; second++ {
		result = tracker.Update(Pose{}, cloud, []RegistrationFit{first, other}, now.Add(time.Duration(second)*time.Second))
		if result.Selected != -1 || result.IndependentViews != 1 {
			t.Fatalf("replay gained confidence at %d seconds: %+v", second, result)
		}
	}
}

func TestViewExpiryPreservesRecentIndependentEvidence(t *testing.T) {
	now := time.Now()
	tracker := PoseTracker{}
	tracker.Update(Pose{}, expiryCloud(0), nil, now)
	tracker.Update(Pose{Yaw: .5}, expiryCloud(2), nil, now.Add(20*time.Second))
	result := tracker.Update(Pose{Yaw: .5}, expiryCloud(2), nil, now.Add(30*time.Second))
	if result.IndependentViews != 1 || !tracker.views[0].at.Equal(now.Add(20*time.Second)) {
		t.Fatal("expiry did not retain only the recent independent view")
	}
	result = tracker.Update(Pose{Yaw: .5}, expiryCloud(2), nil, now.Add(50*time.Second))
	if result.IndependentViews != 1 || !tracker.views[0].at.Equal(now.Add(50*time.Second)) {
		t.Fatal("replayed packets renewed the second view")
	}
	result = tracker.Update(Pose{}, expiryCloud(0), nil, now.Add(-time.Second))
	if result.IndependentViews != 1 || !tracker.views[0].at.Equal(now.Add(-time.Second)) {
		t.Fatal("clock rollback retained future evidence")
	}
}
