package data

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The Robot Operating System 2 (ROS 2) recorder writes ONE bag per Data
// Distribution Service (DDS) domain, however many topics on that domain the
// campaign selected, and names it for the domain. These are the exact paths
// the agent's ROS 2 adapter creates in startOne: the bag under
// ros2/<safeName(domain identifier)>/ and the clock samples beside it as
// ros2/<safeName(domain identifier)>-clock_samples.jsonl.
const (
	ros2TestRMW      = "rmw_cyclonedds_cpp"
	ros2TestDomainNo = 42
)

func writeROS2Bag(t *testing.T, episodeDir string) {
	t.Helper()
	key := safeName(ROS2DomainSourceID(ros2TestRMW, ros2TestDomainNo))
	bag := filepath.Join(episodeDir, "ros2", key)
	if err := os.MkdirAll(bag, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{
		filepath.Join(bag, "metadata.yaml"):                           "rosbag2_bagfile_information:\n  version: 5\n",
		filepath.Join(bag, "bag_0.db3"):                               "sqlite",
		filepath.Join(episodeDir, "ros2", key+"-clock_samples.jsonl"): "{\"episode_nanos\":1}\n",
	} {
		if err := os.WriteFile(name, []byte(contents), 0o640); err != nil {
			t.Fatal(err)
		}
	}
}

// TestSealAttributesROS2BagToEveryTopicSource pins what a sealed manifest says
// a ROS 2 bag belongs to.
//
// The bag's path component is safeName of the DOMAIN identifier, and safeName
// is not invertible, so reading a source identifier back out of it produced
// "ros2_rmw_cyclonedds_cpp_domain-42": a string that matches no source in the
// manifest, no source on the device and nothing in the cloud catalog the
// transfer worker forwards it to. A campaign that selects individual topics is
// the ordinary case since per-topic sources exist, so that was the ordinary
// outcome, and the clock sidecar was labelled with its own file name.
func TestSealAttributesROS2BagToEveryTopicSource(t *testing.T) {
	domain := ROS2DomainSourceID(ros2TestRMW, ros2TestDomainNo)
	lidar := ROS2TopicSourceID(ros2TestRMW, ros2TestDomainNo, "/lidar/points")
	image := ROS2TopicSourceID(ros2TestRMW, ros2TestDomainNo, "/camera/image_raw")
	bagKey := safeName(domain)

	cases := []struct {
		name           string
		selected       []string
		wantPrimary    string
		wantAdditional []string
	}{
		// The finding's case: no domain source is declared at all, so the bag
		// belongs to the topic sources and to nothing else.
		{"topics only", []string{lidar, image}, lidar, []string{image}},
		// The pre-per-topic spelling, which must keep behaving as it did.
		{"domain only", []string{domain}, domain, nil},
		// Both granularities selected: the domain source is the one identifier
		// that covers the whole bag, so it is the primary.
		{"domain and topics", []string{domain, lidar, image}, domain, []string{lidar, image}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := NewManager(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			declared := append([]string{"applications"}, tc.selected...)
			m.SetSourceProvider(func(context.Context) []Source {
				var out []Source
				for _, id := range tc.selected {
					out = append(out, Source{ID: id, Kind: "ros2", ClockDomain: "CLOCK_BOOTTIME", Healthy: true})
				}
				return out
			})
			if _, err = m.Start(StartOptions{Name: "ros2", Sources: declared}); err != nil {
				t.Fatal(err)
			}
			session, ok := m.ActiveSession(AdHocEpisodeKey)
			if !ok {
				t.Fatal("no active session")
			}
			writeROS2Bag(t, session.Directory)
			sealed, err := m.Stop(AdHocEpisodeKey)
			if err != nil {
				t.Fatal(err)
			}

			for _, rel := range []string{
				"ros2/" + bagKey + "/metadata.yaml",
				"ros2/" + bagKey + "/bag_0.db3",
				"ros2/" + bagKey + "-clock_samples.jsonl",
			} {
				entry, listed := fileByPath(sealed.Files, rel)
				if !listed {
					t.Fatalf("manifest does not list %s: %+v", rel, sealed.Files)
				}
				if entry.SourceID != tc.wantPrimary {
					t.Errorf("%s source_id %q, want %q", rel, entry.SourceID, tc.wantPrimary)
				}
				if !slices.Equal(entry.AdditionalSourceIDs, tc.wantAdditional) {
					t.Errorf("%s additional_source_ids %v, want %v", rel, entry.AdditionalSourceIDs, tc.wantAdditional)
				}
			}

			// Nothing in the manifest may name a source the manifest does not
			// declare. This is the assertion that fails on a path fragment,
			// whichever fragment a future layout produces.
			declaredIDs := map[string]bool{}
			for _, stats := range sealed.Sources {
				declaredIDs[stats.Source.ID] = true
			}
			for _, f := range sealed.Files {
				for _, id := range append([]string{f.SourceID}, f.AdditionalSourceIDs...) {
					if id != "" && !declaredIDs[id] {
						t.Errorf("file %s names source %q, which the episode never declared", f.Path, id)
					}
				}
			}
		})
	}
}

// TestSealLeavesUnclaimedFileWithoutASource pins the other half: a file no
// declared source claims carries no source identifier at all. An empty field
// says "unattributed", which a consumer can act on; a mangled path component
// says "attributed to something that does not exist", which it cannot.
func TestSealLeavesUnclaimedFileWithoutASource(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Start(StartOptions{Sources: []string{"applications"}}); err != nil {
		t.Fatal(err)
	}
	session, ok := m.ActiveSession(AdHocEpisodeKey)
	if !ok {
		t.Fatal("no active session")
	}
	writeROS2Bag(t, session.Directory)
	sealed, err := m.Stop(AdHocEpisodeKey)
	if err != nil {
		t.Fatal(err)
	}
	rel := "ros2/" + safeName(ROS2DomainSourceID(ros2TestRMW, ros2TestDomainNo)) + "/metadata.yaml"
	entry, listed := fileByPath(sealed.Files, rel)
	if !listed {
		t.Fatalf("manifest does not list %s", rel)
	}
	if entry.SourceID != "" {
		t.Errorf("%s source_id %q, want it empty: no declared source claims this file", rel, entry.SourceID)
	}
	// The fixed-name files keep their sources, which are not path derived.
	events, listed := fileByPath(sealed.Files, "events.jsonl")
	if !listed || events.SourceID != "applications" {
		t.Errorf("events.jsonl source_id %q, want applications", events.SourceID)
	}
}
