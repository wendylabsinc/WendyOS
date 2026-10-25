package cloud

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"

	pb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/cloud/internal/cloudpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// NotificationSeverity is a basic inbox notification's importance.
type NotificationSeverity int32

// Notification severities retain the Cloud v2 wire values.
const (
	NotificationSeverityUnspecified NotificationSeverity = iota
	NotificationSeverityInfo
	NotificationSeverityWarning
	NotificationSeverityError
	NotificationSeverityCritical
)

// NotificationReadStatus is independent of notification deletion.
type NotificationReadStatus int32

// NotificationRead and NotificationUnread are the two valid read states.
const (
	NotificationRead   NotificationReadStatus = 1
	NotificationUnread NotificationReadStatus = 2
)

// NotificationQuery is applied unchanged to each page or count request.
// Nil fields include all severities or both read states, respectively.
type NotificationQuery struct {
	Severity   *NotificationSeverity
	ReadStatus *NotificationReadStatus
}

// NotificationPage selects a stable canonical-ID-relative window, newest first.
// BeforeID and AfterID are independent and may be combined. Empty means absent.
// A nil Limit defaults to 30; an explicit limit must be between 1 and 100.
type NotificationPage struct {
	BeforeID string
	AfterID  string
	Limit    *int
}

// Notification is the basic Cloud v2 inbox projection. IDs are normalized UUIDs;
// OrganizationID is the immutable tenant UUID. DeepLink is empty or a validated
// wendy:// URL. Metadata distinguishes absence (nil) from an empty object and
// contains nil, float64, string, bool, map[string]any and []any values.
// Rich sections, attachments, previews and controls are deliberately inactive.
type Notification struct {
	ID             string
	OrganizationID string
	Title          string
	Body           string
	Severity       NotificationSeverity
	DeepLink       string
	Metadata       map[string]any
	CreatedAt      *time.Time
	ReadAt         *time.Time
}

// ReadStatus derives the server-owned read state from ReadAt's presence.
func (n Notification) ReadStatus() NotificationReadStatus {
	if n.ReadAt == nil {
		return NotificationUnread
	}
	return NotificationRead
}

// NotificationBatch is a bounded newest-first page. HasMore describes the
// requested traversal direction; no offset or snapshot is synthesized by Core.
type NotificationBatch struct {
	Notifications []Notification
	HasMore       bool
}

// NotificationErrorKind identifies a local inbox validation or timeout failure.
type NotificationErrorKind string

// Notification error kinds preserve the existing basic Companion errors.
const (
	NotificationMissingID           NotificationErrorKind = "missing_canonical_notification_id"
	NotificationInvalidID           NotificationErrorKind = "invalid_canonical_notification_id"
	NotificationInvalidOrganization NotificationErrorKind = "invalid_organization_id"
	NotificationInvalidDeepLink     NotificationErrorKind = "invalid_notification_deep_link"
	NotificationInvalidLimit        NotificationErrorKind = "invalid_batch_limit"
	NotificationInvalidSeverity     NotificationErrorKind = "invalid_severity_filter"
	NotificationInvalidReadStatus   NotificationErrorKind = "invalid_read_status"
	NotificationTimedOut            NotificationErrorKind = "request_timed_out"
)

// NotificationError carries the offending value or limit. A timeout keeps its
// original cause, including RPC status/metadata, through Unwrap.
type NotificationError struct {
	Kind  NotificationErrorKind
	Value string
	Limit int
	Cause error
}

func (e *NotificationError) Error() string {
	switch e.Kind {
	case NotificationMissingID:
		return "Wendy Cloud returned a notification without its canonical ID."
	case NotificationInvalidID:
		return fmt.Sprintf("Wendy Cloud returned an invalid canonical notification ID: %s.", e.Value)
	case NotificationInvalidOrganization:
		return fmt.Sprintf("The Wendy organization ID is invalid: %s.", e.Value)
	case NotificationInvalidDeepLink:
		return fmt.Sprintf("Wendy Cloud returned an invalid notification deep link: %s.", e.Value)
	case NotificationInvalidLimit:
		return fmt.Sprintf("Notification batch limits must be between 1 and 100; received %d.", e.Limit)
	case NotificationInvalidSeverity:
		return "The unspecified notification severity cannot be used as a filter."
	case NotificationInvalidReadStatus:
		return fmt.Sprintf("The notification read status is invalid: %s.", e.Value)
	case NotificationTimedOut:
		return "Wendy Cloud took too long to load. Try again later."
	default:
		return string(e.Kind)
	}
}

// Unwrap preserves the original host failure for errors.Is/errors.As.
func (e *NotificationError) Unwrap() error { return e.Cause }

const notificationService = "/wendycloud.v2.NotificationService/"
const notificationMutationLimit = 200

// GetNotification retrieves exactly the requested canonical notification. The
// host must obtain fresh exact-method Cloud authorization on every RPC through
// Client.Transport; these typed inbox RPCs do not use Signer.Invoke's envelope.
func (c *Client) GetNotification(ctx context.Context, id string) (*Notification, error) {
	id, err := notificationID(id)
	if err != nil {
		return nil, err
	}
	reply := new(pb.Notification)
	if err := c.notificationUnary(ctx, "GetNotification", &pb.GetNotificationRequest{NotificationId: proto.String(id)}, reply); err != nil {
		return nil, err
	}
	n, err := notificationModel(reply)
	if err != nil {
		return nil, err
	}
	if n.ID != id {
		return nil, &NotificationError{Kind: NotificationInvalidID, Value: reply.GetNotificationId()}
	}
	return &n, nil
}

// ListNotifications returns one stable page, preserving the caller's filters
// and both anchors. Invalid rows or another tenant fail the entire page.
func (c *Client) ListNotifications(ctx context.Context, organization string, query NotificationQuery, page NotificationPage) (*NotificationBatch, error) {
	limit := 30
	if page.Limit != nil {
		limit = *page.Limit
	}
	if limit < 1 || limit > 100 {
		return nil, &NotificationError{Kind: NotificationInvalidLimit, Limit: limit}
	}
	if err := validateNotificationQuery(organization, query); err != nil {
		return nil, err
	}
	request := &pb.ListNotificationsV2Request{OrganizationId: organization, Limit: int32(limit), SeverityFilter: notificationSeverityFilter(query.Severity), ReadStatus: notificationReadFilter(query.ReadStatus)}
	for _, anchor := range []struct {
		value  string
		target **string
	}{{page.BeforeID, &request.BeforeNotificationId}, {page.AfterID, &request.AfterNotificationId}} {
		if anchor.value == "" {
			continue
		}
		id, err := notificationID(anchor.value)
		if err != nil {
			return nil, err
		}
		*anchor.target = proto.String(id)
	}
	reply := new(pb.ListNotificationsV2Response)
	if err := c.notificationUnary(ctx, "ListNotificationsV2", request, reply); err != nil {
		return nil, err
	}
	batch := &NotificationBatch{Notifications: make([]Notification, 0, len(reply.Notifications)), HasMore: reply.MoreAvailable}
	for _, row := range reply.Notifications {
		n, err := notificationModel(row)
		if err != nil {
			return nil, err
		}
		if n.OrganizationID != organization {
			return nil, &NotificationError{Kind: NotificationInvalidOrganization, Value: organization}
		}
		batch.Notifications = append(batch.Notifications, n)
	}
	return batch, nil
}

// CountNotifications counts the same severity/read-status selection as a list.
func (c *Client) CountNotifications(ctx context.Context, organization string, query NotificationQuery) (int, error) {
	if err := validateNotificationQuery(organization, query); err != nil {
		return 0, err
	}
	reply := new(pb.CountNotificationsResponse)
	err := c.notificationUnary(ctx, "CountNotifications", &pb.CountNotificationsRequest{OrganizationId: organization, SeverityFilter: notificationSeverityFilter(query.Severity), ReadStatus: notificationReadFilter(query.ReadStatus)}, reply)
	if err != nil {
		return 0, err
	}
	return int(reply.Count), nil
}

// DeleteNotifications sends at most 200 IDs per call and sums changed counts.
// Empty input returns locally. Failure stops immediately; earlier batches may
// have changed Cloud state and are neither retried nor rolled back.
func (c *Client) DeleteNotifications(ctx context.Context, ids []string) (int, error) {
	ids, err := notificationIDs(ids)
	if err != nil {
		return 0, err
	}
	total := 0
	for start := 0; start < len(ids); start += notificationMutationLimit {
		reply := new(pb.DeleteNotificationsResponse)
		if err := c.notificationUnary(ctx, "DeleteNotifications", &pb.DeleteNotificationsRequest{NotificationIds: ids[start:min(start+notificationMutationLimit, len(ids))]}, reply); err != nil {
			return 0, err
		}
		total += int(reply.DeletedCount)
	}
	return total, nil
}

// SetNotificationReadStatus sends ID batches of at most 200, without treating
// updated_count as a success Boolean. Empty input returns locally. On failure,
// completed batches remain applied and no later batch is sent.
func (c *Client) SetNotificationReadStatus(ctx context.Context, status NotificationReadStatus, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if err := validateNotificationReadStatus(status); err != nil {
		return err
	}
	ids, err := notificationIDs(ids)
	if err != nil {
		return err
	}
	for start := 0; start < len(ids); start += notificationMutationLimit {
		request := &pb.SetNotificationReadStatusRequest{NotificationIds: ids[start:min(start+notificationMutationLimit, len(ids))], ReadStatus: pb.NotificationReadStatus(status)}
		if err := c.notificationUnary(ctx, "SetNotificationReadStatus", request, new(pb.SetNotificationReadStatusResponse)); err != nil {
			return err
		}
	}
	return nil
}

// DeleteNotificationsThrough repeats the original inclusive anchor and severity
// until Cloud clears more_available. Each RPC changes at most 1,000 rows. The
// return value is the sum on success only; failures retain partial side effects.
func (c *Client) DeleteNotificationsThrough(ctx context.Context, organization, throughID string, severity *NotificationSeverity) (int, error) {
	if err := validateNotificationQuery(organization, NotificationQuery{Severity: severity}); err != nil {
		return 0, err
	}
	id, err := notificationID(throughID)
	if err != nil {
		return 0, err
	}
	request := &pb.DeleteNotificationsThroughRequest{OrganizationId: organization, ThroughNotificationId: id, SeverityFilter: notificationSeverityFilter(severity)}
	return notificationThrough(ctx, func() (int32, bool, error) {
		reply := new(pb.DeleteNotificationsThroughResponse)
		err := c.notificationUnary(ctx, "DeleteNotificationsThrough", request, reply)
		return reply.DeletedCount, reply.MoreAvailable, err
	})
}

// SetNotificationReadStatusThrough changes all matching rows at or before the
// original inclusive anchor, repeating it while more_available is true. No
// retry or rollback hides a failed call or cancellation after earlier changes.
func (c *Client) SetNotificationReadStatusThrough(ctx context.Context, status NotificationReadStatus, organization, throughID string, severity *NotificationSeverity) (int, error) {
	if err := validateNotificationQuery(organization, NotificationQuery{Severity: severity, ReadStatus: &status}); err != nil {
		return 0, err
	}
	id, err := notificationID(throughID)
	if err != nil {
		return 0, err
	}
	request := &pb.SetNotificationReadStatusThroughRequest{OrganizationId: organization, ThroughNotificationId: id, SeverityFilter: notificationSeverityFilter(severity), ReadStatus: pb.NotificationReadStatus(status)}
	return notificationThrough(ctx, func() (int32, bool, error) {
		reply := new(pb.SetNotificationReadStatusThroughResponse)
		err := c.notificationUnary(ctx, "SetNotificationReadStatusThrough", request, reply)
		return reply.UpdatedCount, reply.MoreAvailable, err
	})
}

func notificationThrough(ctx context.Context, call func() (int32, bool, error)) (int, error) {
	total := 0
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		count, more, err := call()
		if err != nil {
			return 0, err
		}
		total += int(count)
		if !more {
			return total, nil
		}
	}
}

func (c *Client) notificationUnary(ctx context.Context, method string, request, reply proto.Message) error {
	// Companion applies its ten-second Cloud timeout separately to every RPC.
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err := c.unary(ctx, notificationService+method, request, reply)
	if errors.Is(err, context.DeadlineExceeded) {
		return &NotificationError{Kind: NotificationTimedOut, Cause: err}
	}
	return err
}

func notificationID(value string) (string, error) {
	id := strings.ToLower(value)
	if !canonicalUUID(id) {
		return "", &NotificationError{Kind: NotificationInvalidID, Value: value}
	}
	return id, nil
}

func notificationIDs(ids []string) ([]string, error) {
	result := make([]string, len(ids))
	for i, id := range ids {
		var err error
		result[i], err = notificationID(id)
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func validateNotificationReadStatus(status NotificationReadStatus) error {
	if status != NotificationRead && status != NotificationUnread {
		return &NotificationError{Kind: NotificationInvalidReadStatus, Value: fmt.Sprint(status)}
	}
	return nil
}

func validateNotificationQuery(organization string, query NotificationQuery) error {
	if !canonicalUUID(organization) {
		return &NotificationError{Kind: NotificationInvalidOrganization, Value: organization}
	}
	if query.Severity != nil && (*query.Severity < NotificationSeverityInfo || *query.Severity > NotificationSeverityCritical) {
		return &NotificationError{Kind: NotificationInvalidSeverity}
	}
	if query.ReadStatus != nil {
		return validateNotificationReadStatus(*query.ReadStatus)
	}
	return nil
}

func notificationSeverityFilter(severity *NotificationSeverity) *pb.NotificationSeverity {
	if severity == nil {
		return nil
	}
	value := pb.NotificationSeverity(*severity)
	return &value
}

func notificationReadFilter(status *NotificationReadStatus) *pb.NotificationReadStatus {
	if status == nil {
		return nil
	}
	value := pb.NotificationReadStatus(*status)
	return &value
}

func notificationModel(row *pb.Notification) (Notification, error) {
	if row == nil || row.NotificationId == nil {
		return Notification{}, &NotificationError{Kind: NotificationMissingID}
	}
	id, err := notificationID(row.GetNotificationId())
	if err != nil {
		return Notification{}, err
	}
	if !canonicalUUID(row.OrganizationId) {
		return Notification{}, &NotificationError{Kind: NotificationInvalidOrganization, Value: row.OrganizationId}
	}
	if row.DeepLink != "" && !notificationWendyURL(row.DeepLink) {
		return Notification{}, &NotificationError{Kind: NotificationInvalidDeepLink, Value: row.DeepLink}
	}
	n := Notification{ID: id, OrganizationID: row.OrganizationId, Title: row.Title, Body: row.Body, Severity: NotificationSeverity(row.Severity), DeepLink: row.DeepLink}
	if n.Severity < NotificationSeverityInfo || n.Severity > NotificationSeverityCritical {
		n.Severity = NotificationSeverityUnspecified
	}
	if row.Metadata != nil {
		n.Metadata = notificationMetadata(row.Metadata)
	}
	if row.CreatedAt != nil {
		at := row.CreatedAt.AsTime()
		n.CreatedAt = &at
	}
	if row.ReadAt != nil {
		at := row.ReadAt.AsTime()
		n.ReadAt = &at
	}
	return n, nil
}

func notificationWendyURL(value string) bool {
	if strings.IndexFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return false
	}
	u, err := url.Parse(value)
	// net/url normalizes scheme case; Foundation's exact URL check preserves it.
	return err == nil && strings.EqualFold(u.Scheme, "wendy") && u.User == nil && u.Hostname() != "" && u.String()[len(u.Scheme):] == value[len(u.Scheme):]
}

func notificationMetadata(value *structpb.Struct) map[string]any {
	fields := make(map[string]any, len(value.GetFields()))
	for key, value := range value.GetFields() {
		fields[key] = notificationMetadataValue(value)
	}
	return fields
}

func notificationMetadataValue(value *structpb.Value) any {
	switch kind := value.GetKind().(type) {
	case *structpb.Value_NumberValue:
		return kind.NumberValue
	case *structpb.Value_StringValue:
		return kind.StringValue
	case *structpb.Value_BoolValue:
		return kind.BoolValue
	case *structpb.Value_StructValue:
		return notificationMetadata(kind.StructValue)
	case *structpb.Value_ListValue:
		values := make([]any, len(kind.ListValue.GetValues()))
		for i, value := range kind.ListValue.GetValues() {
			values[i] = notificationMetadataValue(value)
		}
		return values
	default:
		return nil
	}
}
