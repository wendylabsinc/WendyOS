package cloud

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	pb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/cloud/internal/cloudpb"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform/platformtest"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const notificationFixtureID = "97f49824-2239-478a-b2dd-d09382f288e1"

func notificationFixture() *pb.Notification {
	return &pb.Notification{NotificationId: proto.String(notificationFixtureID), OrganizationId: tenant, Title: "Fire detected", Body: "Potential fire detected", Severity: pb.NotificationSeverity_NOTIFICATION_SEVERITY_CRITICAL}
}

func notificationClient(t *testing.T, call func(context.Context, platform.RPCRequest) (proto.Message, error)) *Client {
	t.Helper()
	return &Client{Transport: platformtest.Transport{UnaryFunc: func(ctx context.Context, request platform.RPCRequest) (platform.RPCResponse, error) {
		if !strings.HasPrefix(request.Method, notificationService) {
			t.Fatalf("unexpected method %s", request.Method)
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 10*time.Second {
			t.Fatalf("missing/bad per-call timeout %v", deadline)
		}
		reply, err := call(ctx, request)
		if err != nil {
			return platform.RPCResponse{}, err
		}
		return platform.RPCResponse{Message: wire(t, reply)}, nil
	}}}
}

func decodeNotificationRequest(t *testing.T, request platform.RPCRequest, method string, message proto.Message) {
	t.Helper()
	if request.Method != notificationService+method {
		t.Fatalf("method %s, want %s", request.Method, method)
	}
	if err := proto.Unmarshal(request.Message, message); err != nil {
		t.Fatal(err)
	}
}

func TestNotificationGetMapsBasicModelAndIdentity(t *testing.T) {
	row := notificationFixture()
	row.CreatedAt = &timestamppb.Timestamp{Seconds: 1753000000, Nanos: 123456789}
	row.ReadAt = &timestamppb.Timestamp{Seconds: 1753000100}
	row.DeletedAt = &timestamppb.Timestamp{Seconds: 1753000200}
	row.DeepLink = "wendy://device/robot?tab=apps"
	row.Metadata, _ = structpb.NewStruct(map[string]any{"source": "sensor", "n": 4.5, "active": true, "nested": map[string]any{"list": []any{nil, "text", false}}})
	// Wire-compatible rich fields, including malformed values, remain inactive.
	row.Sections = []*pb.NotificationSection{{Key: "event", Title: proto.String("Event")}}
	row.Attachments = []*pb.NotificationAttachment{{Id: "not-a-uuid", Url: "https://media.invalid"}}
	row.PreviewAttachmentId = proto.String("not-a-uuid")
	row.Controls = []*pb.NotificationControl{{}}
	c := notificationClient(t, func(_ context.Context, request platform.RPCRequest) (proto.Message, error) {
		var get pb.GetNotificationRequest
		decodeNotificationRequest(t, request, "GetNotification", &get)
		if get.Id != "" || get.NotificationId == nil || get.GetNotificationId() != notificationFixtureID {
			t.Fatalf("not a canonical typed Get: %v", &get)
		}
		return row, nil
	})
	n, err := c.GetNotification(context.Background(), strings.ToUpper(notificationFixtureID))
	if err != nil {
		t.Fatal(err)
	}
	if n.ID != notificationFixtureID || n.OrganizationID != tenant || n.Title != row.Title || n.Body != row.Body || n.Severity != NotificationSeverityCritical || n.DeepLink != row.DeepLink || n.ReadStatus() != NotificationRead || !n.CreatedAt.Equal(row.CreatedAt.AsTime()) || !n.ReadAt.Equal(row.ReadAt.AsTime()) {
		t.Fatalf("model changed: %+v", n)
	}
	if !reflect.DeepEqual(n.Metadata, row.Metadata.AsMap()) {
		t.Fatalf("metadata %v", n.Metadata)
	}
	// The public model has no rich activation fields, even if the server sends them.
	for _, name := range []string{"Sections", "Attachments", "PreviewAttachmentID", "Controls"} {
		if _, ok := reflect.TypeOf(*n).FieldByName(name); ok {
			t.Errorf("rich field activated: %s", name)
		}
	}
}

func TestNotificationMappingPresenceUnknownSeverityAndUUIDNormalization(t *testing.T) {
	row := notificationFixture()
	row.NotificationId = proto.String(strings.ToUpper(notificationFixtureID))
	row.Severity = pb.NotificationSeverity(999)
	n, err := notificationModel(row)
	if err != nil || n.ID != notificationFixtureID || n.Severity != NotificationSeverityUnspecified || n.DeepLink != "" || n.Metadata != nil || n.CreatedAt != nil || n.ReadAt != nil || n.ReadStatus() != NotificationUnread {
		t.Fatalf("model %+v: %v", n, err)
	}
	row.Metadata = &structpb.Struct{}
	row.CreatedAt = &timestamppb.Timestamp{}
	row.ReadAt = &timestamppb.Timestamp{}
	n, err = notificationModel(row)
	if err != nil || n.Metadata == nil || len(n.Metadata) != 0 || n.CreatedAt == nil || n.CreatedAt.Unix() != 0 || n.ReadStatus() != NotificationRead {
		t.Fatalf("presence %+v: %v", n, err)
	}
	row.Metadata.Fields = map[string]*structpb.Value{"missing": {}, "number": structpb.NewNumberValue(math.Inf(1))}
	n, err = notificationModel(row)
	if err != nil || n.Metadata["missing"] != nil || !math.IsInf(n.Metadata["number"].(float64), 1) {
		t.Fatalf("metadata kinds %+v: %v", n, err)
	}
}

func TestNotificationRejectsMalformedModelsAndWrongGetIdentity(t *testing.T) {
	cases := []struct {
		name string
		edit func(*pb.Notification)
		kind NotificationErrorKind
	}{
		{"absent ID", func(n *pb.Notification) { n.NotificationId = nil }, NotificationMissingID},
		{"empty ID", func(n *pb.Notification) { n.NotificationId = proto.String("") }, NotificationInvalidID},
		{"projection ID", func(n *pb.Notification) { n.NotificationId = proto.String("42") }, NotificationInvalidID},
		{"compact UUID", func(n *pb.Notification) {
			n.NotificationId = proto.String(strings.ReplaceAll(notificationFixtureID, "-", ""))
		}, NotificationInvalidID},
		{"wrong Get ID", func(n *pb.Notification) { n.NotificationId = proto.String(assetID) }, NotificationInvalidID},
		{"wrong namespace", func(n *pb.Notification) { n.OrganizationId = "org-alpha" }, NotificationInvalidOrganization},
		{"uppercase tenant", func(n *pb.Notification) { n.OrganizationId = strings.ToUpper(tenant) }, NotificationInvalidOrganization},
	}
	for _, link := range []string{"https://example.com", "wendy:path", "wendy:///path", "wendy://user:password@device", "wendy://device/a b", "wendy://device/%zz", "wendy://device/\n"} {
		cases = append(cases, struct {
			name string
			edit func(*pb.Notification)
			kind NotificationErrorKind
		}{link, func(n *pb.Notification) { n.DeepLink = link }, NotificationInvalidDeepLink})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := notificationFixture()
			tc.edit(row)
			c := notificationClient(t, func(context.Context, platform.RPCRequest) (proto.Message, error) { return row, nil })
			n, err := c.GetNotification(context.Background(), notificationFixtureID)
			var failure *NotificationError
			if n != nil || !errors.As(err, &failure) || failure.Kind != tc.kind {
				t.Fatalf("notification %+v, error %v", n, err)
			}
		})
	}
	for _, link := range []string{"wendy://device", "WENDY://device/path", "wendy://device/a%20b?x=%2F#info"} {
		if !notificationWendyURL(link) {
			t.Errorf("valid link refused %q", link)
		}
	}
}

func TestNotificationListPreservesAnchorsFiltersAndNewestFirstOrder(t *testing.T) {
	for _, page := range []NotificationPage{{}, {BeforeID: notificationFixtureID}, {AfterID: notificationFixtureID}, {BeforeID: notificationFixtureID, AfterID: assetID, Limit: intPointer(100)}} {
		t.Run(fmt.Sprint(page.BeforeID, "/", page.AfterID), func(t *testing.T) {
			severity, status := NotificationSeverityWarning, NotificationUnread
			query := NotificationQuery{Severity: &severity, ReadStatus: &status}
			for i := 0; i < 2; i++ {
				c := notificationClient(t, func(_ context.Context, request platform.RPCRequest) (proto.Message, error) {
					var list pb.ListNotificationsV2Request
					decodeNotificationRequest(t, request, "ListNotificationsV2", &list)
					wantLimit := 30
					if page.Limit != nil {
						wantLimit = *page.Limit
					}
					if list.OrganizationId != tenant || list.GetBeforeNotificationId() != page.BeforeID || list.GetAfterNotificationId() != page.AfterID || (list.BeforeNotificationId != nil) != (page.BeforeID != "") || (list.AfterNotificationId != nil) != (page.AfterID != "") || int(list.Limit) != wantLimit || list.SeverityFilter == nil || list.GetSeverityFilter() != pb.NotificationSeverity(severity) || list.ReadStatus == nil || list.GetReadStatus() != pb.NotificationReadStatus(status) {
						t.Fatalf("query changed: %v", &list)
					}
					first, second := notificationFixture(), notificationFixture()
					second.NotificationId = proto.String(assetID)
					return &pb.ListNotificationsV2Response{Notifications: []*pb.Notification{first, second}, MoreAvailable: true}, nil
				})
				batch, err := c.ListNotifications(context.Background(), tenant, query, page)
				if err != nil || !batch.HasMore || len(batch.Notifications) != 2 || batch.Notifications[0].ID != notificationFixtureID || batch.Notifications[1].ID != assetID {
					t.Fatalf("batch %+v, error %v", batch, err)
				}
			}
		})
	}
}

func intPointer(value int) *int { return &value }

func TestNotificationListFailsEntireBatchForInvalidOrForeignRows(t *testing.T) {
	for _, bad := range []*pb.Notification{{}, {NotificationId: proto.String(assetID), OrganizationId: assetID}} {
		c := notificationClient(t, func(context.Context, platform.RPCRequest) (proto.Message, error) {
			return &pb.ListNotificationsV2Response{Notifications: []*pb.Notification{notificationFixture(), bad}, MoreAvailable: true}, nil
		})
		batch, err := c.ListNotifications(context.Background(), tenant, NotificationQuery{}, NotificationPage{})
		if batch != nil || err == nil {
			t.Fatalf("partial result %+v: %v", batch, err)
		}
	}
	c := notificationClient(t, func(_ context.Context, request platform.RPCRequest) (proto.Message, error) {
		var list pb.ListNotificationsV2Request
		decodeNotificationRequest(t, request, "ListNotificationsV2", &list)
		if list.SeverityFilter != nil || list.ReadStatus != nil {
			t.Fatal("absent filters became explicit")
		}
		return &pb.ListNotificationsV2Response{}, nil
	})
	batch, err := c.ListNotifications(context.Background(), tenant, NotificationQuery{}, NotificationPage{Limit: intPointer(1)})
	if err != nil || batch.Notifications == nil || len(batch.Notifications) != 0 || batch.HasMore {
		t.Fatalf("empty result %+v: %v", batch, err)
	}
}

func TestNotificationValidationPrecedesHostAndEmptyMutationsAreLocal(t *testing.T) {
	c := &Client{Transport: platformtest.Transport{UnaryFunc: func(context.Context, platform.RPCRequest) (platform.RPCResponse, error) {
		t.Fatal("unexpected credential/transport access")
		return platform.RPCResponse{}, nil
	}}}
	assertKind := func(err error, kind NotificationErrorKind) {
		t.Helper()
		var failure *NotificationError
		if !errors.As(err, &failure) || failure.Kind != kind {
			t.Fatalf("want %s: %v", kind, err)
		}
	}
	ctx := context.Background()
	for _, limit := range []int{-1, 0, 101} {
		_, err := c.ListNotifications(ctx, tenant, NotificationQuery{}, NotificationPage{Limit: &limit})
		assertKind(err, NotificationInvalidLimit)
	}
	for _, org := range []string{"", "42", "org-alpha", strings.ToUpper(tenant)} {
		_, err := c.ListNotifications(ctx, org, NotificationQuery{}, NotificationPage{})
		assertKind(err, NotificationInvalidOrganization)
		_, err = c.CountNotifications(ctx, org, NotificationQuery{})
		assertKind(err, NotificationInvalidOrganization)
		_, err = c.DeleteNotificationsThrough(ctx, org, notificationFixtureID, nil)
		assertKind(err, NotificationInvalidOrganization)
		_, err = c.SetNotificationReadStatusThrough(ctx, NotificationRead, org, notificationFixtureID, nil)
		assertKind(err, NotificationInvalidOrganization)
	}
	for _, severity := range []NotificationSeverity{NotificationSeverityUnspecified, -1, 99} {
		_, err := c.ListNotifications(ctx, tenant, NotificationQuery{Severity: &severity}, NotificationPage{})
		assertKind(err, NotificationInvalidSeverity)
		_, err = c.CountNotifications(ctx, tenant, NotificationQuery{Severity: &severity})
		assertKind(err, NotificationInvalidSeverity)
		_, err = c.DeleteNotificationsThrough(ctx, tenant, notificationFixtureID, &severity)
		assertKind(err, NotificationInvalidSeverity)
		_, err = c.SetNotificationReadStatusThrough(ctx, NotificationRead, tenant, notificationFixtureID, &severity)
		assertKind(err, NotificationInvalidSeverity)
	}
	for _, status := range []NotificationReadStatus{0, -1, 99} {
		_, err := c.ListNotifications(ctx, tenant, NotificationQuery{ReadStatus: &status}, NotificationPage{})
		assertKind(err, NotificationInvalidReadStatus)
		_, err = c.CountNotifications(ctx, tenant, NotificationQuery{ReadStatus: &status})
		assertKind(err, NotificationInvalidReadStatus)
		assertKind(c.SetNotificationReadStatus(ctx, status, []string{notificationFixtureID}), NotificationInvalidReadStatus)
		_, err = c.SetNotificationReadStatusThrough(ctx, status, tenant, notificationFixtureID, nil)
		assertKind(err, NotificationInvalidReadStatus)
	}
	for _, id := range []string{"", "42", "not-a-uuid", " " + notificationFixtureID} {
		_, err := c.GetNotification(ctx, id)
		assertKind(err, NotificationInvalidID)
		_, err = c.DeleteNotifications(ctx, []string{notificationFixtureID, id})
		assertKind(err, NotificationInvalidID)
		assertKind(c.SetNotificationReadStatus(ctx, NotificationRead, []string{id}), NotificationInvalidID)
		_, err = c.DeleteNotificationsThrough(ctx, tenant, id, nil)
		assertKind(err, NotificationInvalidID)
		_, err = c.SetNotificationReadStatusThrough(ctx, NotificationRead, tenant, id, nil)
		assertKind(err, NotificationInvalidID)
		if id != "" {
			for _, page := range []NotificationPage{{BeforeID: id}, {AfterID: id}} {
				_, err = c.ListNotifications(ctx, tenant, NotificationQuery{}, page)
				assertKind(err, NotificationInvalidID)
			}
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if count, err := c.DeleteNotifications(ctx, nil); err != nil || count != 0 {
		t.Fatalf("empty delete %d: %v", count, err)
	}
	if err := c.SetNotificationReadStatus(ctx, 0, nil); err != nil {
		t.Fatal(err)
	}
}

func notificationFixtureIDs(count int) []string {
	ids := make([]string, count)
	for i := range ids {
		ids[i] = fmt.Sprintf("%08x-2239-478a-b2dd-d09382f288e1", i)
	}
	return ids
}

func TestNotificationIDMutationBatchBoundariesAndCounts(t *testing.T) {
	for _, count := range []int{1, 199, 200, 201, 400, 401} {
		for _, read := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/read=%t", count, read), func(t *testing.T) {
				ids := notificationFixtureIDs(count)
				var got []string
				calls := 0
				c := notificationClient(t, func(_ context.Context, request platform.RPCRequest) (proto.Message, error) {
					calls++
					var batch []string
					if read {
						var r pb.SetNotificationReadStatusRequest
						decodeNotificationRequest(t, request, "SetNotificationReadStatus", &r)
						batch = r.NotificationIds
						if r.ReadStatus != pb.NotificationReadStatus_NOTIFICATION_READ_STATUS_UNREAD {
							t.Fatal(&r)
						}
					} else {
						var r pb.DeleteNotificationsRequest
						decodeNotificationRequest(t, request, "DeleteNotifications", &r)
						batch = r.NotificationIds
					}
					want := min(200, count-len(got))
					if len(batch) != want {
						t.Fatalf("batch %d, want %d", len(batch), want)
					}
					got = append(got, batch...)
					if read {
						return &pb.SetNotificationReadStatusResponse{UpdatedCount: 0}, nil
					}
					return &pb.DeleteNotificationsResponse{DeletedCount: int32(calls)}, nil
				})
				if read {
					if err := c.SetNotificationReadStatus(context.Background(), NotificationUnread, ids); err != nil {
						t.Fatal(err)
					}
				} else {
					total, err := c.DeleteNotifications(context.Background(), ids)
					if err != nil || total != calls*(calls+1)/2 {
						t.Fatalf("total %d: %v", total, err)
					}
				}
				if calls != (count+199)/200 || !reflect.DeepEqual(ids, got) {
					t.Fatalf("batches reordered/omitted IDs: %d", calls)
				}
			})
		}
	}
	// Keep duplicates: the server owns deduplication and changed-row counting.
	c := notificationClient(t, func(_ context.Context, request platform.RPCRequest) (proto.Message, error) {
		var r pb.DeleteNotificationsRequest
		decodeNotificationRequest(t, request, "DeleteNotifications", &r)
		if !reflect.DeepEqual(r.NotificationIds, []string{notificationFixtureID, notificationFixtureID}) {
			t.Fatal(&r)
		}
		return &pb.DeleteNotificationsResponse{DeletedCount: 1}, nil
	})
	if total, err := c.DeleteNotifications(context.Background(), []string{notificationFixtureID, strings.ToUpper(notificationFixtureID)}); err != nil || total != 1 {
		t.Fatalf("duplicate count %d: %v", total, err)
	}
}

func TestNotificationThroughRepeatsOriginalRequestAndContinuationTagTwo(t *testing.T) {
	for _, counts := range [][]int32{{0}, {1000}, {1000, 1}, {1000, 1000, 1}, {0, 7}} {
		for _, read := range []bool{false, true} {
			t.Run(fmt.Sprintf("%v/read=%t", counts, read), func(t *testing.T) {
				calls := 0
				var first []byte
				severity := NotificationSeverityCritical
				c := &Client{Transport: platformtest.Transport{UnaryFunc: func(ctx context.Context, request platform.RPCRequest) (platform.RPCResponse, error) {
					if calls >= len(counts) {
						t.Fatal("extra continuation")
					}
					if first == nil {
						first = append([]byte(nil), request.Message...)
					} else if !bytes.Equal(first, request.Message) {
						t.Fatal("anchor/filter changed")
					}
					if read {
						var r pb.SetNotificationReadStatusThroughRequest
						decodeNotificationRequest(t, request, "SetNotificationReadStatusThrough", &r)
						if r.OrganizationId != tenant || r.ThroughNotificationId != notificationFixtureID || r.GetSeverityFilter() != pb.NotificationSeverity(severity) || r.ReadStatus != pb.NotificationReadStatus_NOTIFICATION_READ_STATUS_READ {
							t.Fatal(&r)
						}
					} else {
						var r pb.DeleteNotificationsThroughRequest
						decodeNotificationRequest(t, request, "DeleteNotificationsThrough", &r)
						if r.OrganizationId != tenant || r.ThroughNotificationId != notificationFixtureID || r.GetSeverityFilter() != pb.NotificationSeverity(severity) {
							t.Fatal(&r)
						}
					}
					count := counts[calls]
					calls++
					// Encode the published tags directly; absent tag 2 must terminate.
					response := wire(t, &pb.DeleteNotificationsThroughResponse{DeletedCount: count})
					if calls < len(counts) {
						response = append(response, 16, 1)
					}
					return platform.RPCResponse{Message: response}, nil
				}}}
				var total int
				var err error
				if read {
					total, err = c.SetNotificationReadStatusThrough(context.Background(), NotificationRead, tenant, notificationFixtureID, &severity)
				} else {
					total, err = c.DeleteNotificationsThrough(context.Background(), tenant, notificationFixtureID, &severity)
				}
				want := 0
				for _, count := range counts {
					want += int(count)
				}
				if err != nil || total != want || calls != len(counts) {
					t.Fatalf("total %d calls %d: %v", total, calls, err)
				}
			})
		}
	}
}

func notificationOperations() map[string]func(context.Context, *Client) error {
	return map[string]func(context.Context, *Client) error{
		"Get": func(ctx context.Context, c *Client) error {
			_, err := c.GetNotification(ctx, notificationFixtureID)
			return err
		},
		"List": func(ctx context.Context, c *Client) error {
			_, err := c.ListNotifications(ctx, tenant, NotificationQuery{}, NotificationPage{})
			return err
		},
		"Count": func(ctx context.Context, c *Client) error {
			_, err := c.CountNotifications(ctx, tenant, NotificationQuery{})
			return err
		},
		"Delete": func(ctx context.Context, c *Client) error {
			_, err := c.DeleteNotifications(ctx, notificationFixtureIDs(401))
			return err
		},
		"Read": func(ctx context.Context, c *Client) error {
			return c.SetNotificationReadStatus(ctx, NotificationRead, notificationFixtureIDs(401))
		},
		"DeleteThrough": func(ctx context.Context, c *Client) error {
			_, err := c.DeleteNotificationsThrough(ctx, tenant, notificationFixtureID, nil)
			return err
		},
		"ReadThrough": func(ctx context.Context, c *Client) error {
			_, err := c.SetNotificationReadStatusThrough(ctx, NotificationRead, tenant, notificationFixtureID, nil)
			return err
		},
	}
}

func TestNotificationFailurePreservesStatusTrailersWithoutRetry(t *testing.T) {
	failure := &platform.RPCStatusError{Code: platform.RPCPermissionDenied, Message: "not allowed", TrailingMetadata: platform.Metadata{{Name: "details-bin", Value: []byte{0, 1, 255}}}}
	for name, operation := range notificationOperations() {
		t.Run(name, func(t *testing.T) {
			calls := 0
			c := notificationClient(t, func(context.Context, platform.RPCRequest) (proto.Message, error) { calls++; return nil, failure })
			err := operation(context.Background(), c)
			var status *platform.RPCStatusError
			if !errors.As(err, &status) || status != failure || calls != 1 || !bytes.Equal(status.TrailingMetadata[0].Value, []byte{0, 1, 255}) {
				t.Fatalf("lost error/retried: %v calls=%d", err, calls)
			}
		})
	}
}

func TestNotificationCancellationAndLaterMutationFailureStopImmediately(t *testing.T) {
	for name, operation := range notificationOperations() {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := operation(ctx, &Client{Transport: platformtest.Transport{}}); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled call: %v", err)
			}
		})
	}
	for _, name := range []string{"Delete", "Read", "DeleteThrough", "ReadThrough"} {
		for _, cancelAfter := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cancel=%t", name, cancelAfter), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				calls := 0
				failure := errors.New("second call failed")
				c := notificationClient(t, func(context.Context, platform.RPCRequest) (proto.Message, error) {
					calls++
					if calls == 2 {
						return nil, failure
					}
					if calls > 2 {
						t.Fatal("retried or continued")
					}
					if cancelAfter {
						cancel()
					}
					// Same count/continuation tags for both Through responses; ID
					// mutations ignore continuation and still check the next call.
					return &pb.DeleteNotificationsThroughResponse{DeletedCount: 1000, MoreAvailable: true}, nil
				})
				err := notificationOperations()[name](ctx, c)
				want, wantCalls := failure, 2
				if cancelAfter {
					want, wantCalls = context.Canceled, 1
				}
				if !errors.Is(err, want) || calls != wantCalls {
					t.Fatalf("error %v calls %d", err, calls)
				}
			})
		}
	}
}

func TestNotificationDeadlineMappingAndMalformedResponse(t *testing.T) {
	for _, failure := range []error{context.DeadlineExceeded, &platform.RPCStatusError{Code: platform.RPCDeadlineExceeded, Message: "deadline"}} {
		c := notificationClient(t, func(context.Context, platform.RPCRequest) (proto.Message, error) { return nil, failure })
		_, err := c.CountNotifications(context.Background(), tenant, NotificationQuery{})
		var timeout *NotificationError
		if !errors.As(err, &timeout) || timeout.Kind != NotificationTimedOut || !errors.Is(err, failure) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("lost timeout cause: %v", err)
		}
	}
	c := &Client{Transport: platformtest.Transport{UnaryFunc: func(context.Context, platform.RPCRequest) (platform.RPCResponse, error) {
		return platform.RPCResponse{Message: []byte{255}}, nil
	}}}
	if n, err := c.GetNotification(context.Background(), notificationFixtureID); n != nil || err == nil {
		t.Fatalf("malformed reply %+v: %v", n, err)
	}
	if _, err := (&Client{}).CountNotifications(context.Background(), tenant, NotificationQuery{}); err == nil {
		t.Fatal("missing host accepted")
	}
}

func TestNotificationFreshExactMethodHostAuthenticationForEveryRPC(t *testing.T) {
	entropy := &testEntropy{}
	signatures := 0
	var tokens []string
	var proofs []string
	key := platformtest.Signer{
		AlgorithmFunc: func() platform.SignatureAlgorithm { return platform.MLDSA65 },
		PublicKeyFunc: func(context.Context) ([]byte, error) {
			return asn1.Marshal(struct {
				Algorithm struct{ OID asn1.ObjectIdentifier }
				PublicKey asn1.BitString
			}{Algorithm: struct{ OID asn1.ObjectIdentifier }{asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 3, 18}}, PublicKey: asn1.BitString{Bytes: make([]byte, 1952), BitLength: 1952 * 8}})
		},
		SignFunc: func(ctx context.Context, data []byte) ([]byte, error) {
			signatures++
			return append([]byte(nil), data...), ctx.Err()
		},
	}
	for name, operation := range notificationOperations() {
		t.Run(name, func(t *testing.T) {
			calls := 0
			c := notificationClient(t, func(ctx context.Context, request platform.RPCRequest) (proto.Message, error) {
				calls++
				// Host refreshes the Cloud-resource token and signs once per call;
				// Core passes typed requests without a SignedRequest envelope.
				token := fmt.Sprintf("cloud-resource-token-%d", signatures)
				metadata, err := AuthenticationMetadata(ctx, entropy, "cloud.example:443", request.Method, token, key, request.Metadata)
				if err != nil {
					t.Fatal(err)
				}
				tokens = append(tokens, token)
				proofs = append(proofs, string(metadata[len(metadata)-1].Value))
				_, payload := claims(t, metadata[len(metadata)-1].Value)
				hash := sha256.Sum256([]byte(token))
				if payload["htu"] != "https://cloud.example:443"+request.Method || payload["htm"] != "POST" || payload["ath"] != base64.RawURLEncoding.EncodeToString(hash[:]) || string(metadata[len(metadata)-2].Value) != "DPoP "+token {
					t.Fatalf("wrong authorization target: %v", payload)
				}
				switch request.Method {
				case notificationService + "GetNotification":
					return notificationFixture(), nil
				case notificationService + "ListNotificationsV2":
					return &pb.ListNotificationsV2Response{}, nil
				case notificationService + "CountNotifications":
					var count pb.CountNotificationsRequest
					decodeNotificationRequest(t, request, "CountNotifications", &count)
					if count.OrganizationId != tenant || count.SeverityFilter != nil || count.ReadStatus != nil {
						t.Fatal(&count)
					}
					return &pb.CountNotificationsResponse{Count: 17}, nil
				case notificationService + "DeleteNotifications":
					return &pb.DeleteNotificationsResponse{}, nil
				case notificationService + "SetNotificationReadStatus":
					return &pb.SetNotificationReadStatusResponse{}, nil
				default:
					return &pb.DeleteNotificationsThroughResponse{DeletedCount: 1, MoreAvailable: calls < 2}, nil
				}
			})
			if err := operation(context.Background(), c); err != nil {
				t.Fatal(err)
			}
			want := 1
			if name == "Delete" || name == "Read" {
				want = 3
			}
			if strings.HasSuffix(name, "Through") {
				want = 2
			}
			if calls != want {
				t.Fatalf("calls %d, want %d", calls, want)
			}
		})
	}
	if signatures != len(tokens) {
		t.Fatal("not signed for each RPC")
	}
	for i := 1; i < len(proofs); i++ {
		if proofs[i] == proofs[i-1] || tokens[i] == tokens[i-1] {
			t.Fatal("authorization reused")
		}
	}
}

func TestNotificationCountUsesFiltersAndReturnsServerCount(t *testing.T) {
	severity, status := NotificationSeverityInfo, NotificationRead
	c := notificationClient(t, func(_ context.Context, request platform.RPCRequest) (proto.Message, error) {
		var count pb.CountNotificationsRequest
		decodeNotificationRequest(t, request, "CountNotifications", &count)
		if count.OrganizationId != tenant || count.GetSeverityFilter() != pb.NotificationSeverity(severity) || count.GetReadStatus() != pb.NotificationReadStatus(status) {
			t.Fatal(&count)
		}
		return &pb.CountNotificationsResponse{Count: 17}, nil
	})
	if count, err := c.CountNotifications(context.Background(), tenant, NotificationQuery{Severity: &severity, ReadStatus: &status}); err != nil || count != 17 {
		t.Fatalf("count %d: %v", count, err)
	}
}

func TestNotificationHostCancellationInFlightAndEarlierDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	c := notificationClient(t, func(ctx context.Context, _ platform.RPCRequest) (proto.Message, error) {
		close(started)
		<-ctx.Done()
		return nil, &platform.RPCStatusError{Code: platform.RPCCanceled, Cause: ctx.Err()}
	})
	finished := make(chan error, 1)
	go func() { _, err := c.GetNotification(ctx, notificationFixtureID); finished <- err }()
	<-started
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("in-flight cancellation lost: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	ctx, stop := context.WithDeadline(context.Background(), deadline)
	defer stop()
	c = notificationClient(t, func(ctx context.Context, _ platform.RPCRequest) (proto.Message, error) {
		if got, ok := ctx.Deadline(); !ok || !got.Equal(deadline) {
			t.Fatalf("parent deadline extended: %v", got)
		}
		return &pb.CountNotificationsResponse{}, nil
	})
	if _, err := c.CountNotifications(ctx, tenant, NotificationQuery{}); err != nil {
		t.Fatal(err)
	}
}
