# Notifications API

The `NotificationService` gRPC service creates, lists, and manages
operator-facing Wendy Notifications in a Wendy Cloud organization.

## Proto package

`wendycloud.v1` — defined in `Proto/cloud/notifications.proto`.

## App-facing API (`wendy.system.v1`)

Apps with the [`notifications` entitlement](../device/entitlements.md#notifications)
call `wendy.system.v1.NotificationService` over the Unix socket at
`$WENDY_SYSTEM_SOCKET` (`/run/wendy/system/system.sock`). This creates a
canonical Wendy Notification in the recipients' Companion inboxes. Cloud then
attempts APNs delivery; apps do not send an arbitrary APNs payload directly.

WendyKit is currently the only application SDK for this API, and it is
Swift-only. Apps written in other languages call this gRPC service directly.
See [Send notifications from a device app](/docs/guides/device-notifications)
for entitlement and Cloud grant setup, Swift and direct gRPC examples, and
delivery behavior.

The private socket binds every call to trusted app identity. The request cannot
supply an app ID, device ID, or organization ID; the agent adds app identity and
Cloud derives device and organization identity from device mTLS.

### `Send`

```
Send(SendRequest) → SendResponse
```

The agent forwards the request to Cloud. `notification_id` is the caller-chosen resource
identity, not a retry token. After successful creation, every reuse of its
canonical UUID—including an otherwise identical request or a differently cased
spelling—returns `ALREADY_EXISTS`; the prior success is never replayed.

Local validation and rate-limit failures happen before forwarding and do not
claim the UUID. After correcting the request or waiting for the local rate limit,
the caller may retry with the same `notification_id`.

#### `SendRequest`

| Field | Type | Description |
|---|---|---|
| `audience` | `NotificationAudience` | Union of the user, organization team, and organization role selectors below. |
| `title` | `string` | Notification title. |
| `body` | `string` | Notification body. |
| `severity` | `NotificationSeverity` | `INFO`, `WARNING`, `ERROR`, or `CRITICAL`. |
| `deep_link` | `string` | Absolute `wendy://` URI. |
| `notification_id` | `string` | Caller-chosen Notification resource UUID v4. Cloud returns canonical lowercase; any canonical reuse after creation returns `ALREADY_EXISTS`. |
| `metadata` | `optional Struct` | Structured JSON-compatible metadata. |

#### `NotificationAudience`

The selector fields have union semantics. At most 100 selector entries may be
supplied across the lists. Cloud normalizes and deduplicates them, remains authoritative
for recipient resolution, and resolves at most 100 recipients for a device-app
send.

| Field | Type | Description |
|---|---|---|
| `user_ids` | `repeated string` | User IDs to include. |
| `team_ids` | `repeated int32` | Legacy Cloud v1 organization team IDs. |
| `roles` | `repeated OrganizationRole` | Organization roles to include. |
| `team_uuids` | `repeated string` | Canonical Cloud v2 organization team UUIDs. |

Do not set both `team_ids` and `team_uuids`. At least one selector is required.
A user selected through more than one field
receives one Notification.

#### `SendResponse`

| Field | Type | Description |
|---|---|---|
| `notification_id` | `string` | Canonical lowercase UUID of the newly created Notification resource. |

Recipient totals are intentionally omitted because team and role counts can disclose
organization membership.

Device-app sources may create 10 accepted Notifications per minute, and the
agent also smooths bursts locally. Repeated rate-limit violations can quarantine
that app/device source for 15 minutes.
Device-originated deep links are restricted to the source device;
`wendy://devices/current/live` is the portable form. For device-originated APNs
alerts, Cloud uses `Wendy · <Cloud app name>` as the banner title while retaining
the supplied title on the stored Notification.

## `Notification` message

Fields 1–8 are the stable legacy Companion wire contract. V2 adds canonical
identity, content, audience, and `created_by` attribution without changing those
legacy fields.

| Field | Type | Description |
|---|---|---|
| `id` | `int32` | Legacy numeric ID. |
| `user_id` | `string` | Recipient user ID in legacy and per-user read views. |
| `organization_id` | `int32` | Organization ID. |
| `body` | `string` | Notification body. |
| `severity` | `NotificationSeverity` | Severity level. |
| `related_entities` | `Struct` | Legacy structured context. |
| `created_at` | `Timestamp` | Creation time. |
| `deleted_at` | `optional Timestamp` | Soft-deletion time. |
| `title` | `string` | Notification title. |
| `deep_link` | `string` | Absolute `wendy://` URI. |
| `notification_id` | `optional string` | Canonical caller-chosen resource UUID v4; absent on legacy records. |
| `metadata` | `Struct` | Structured JSON-compatible metadata. |
| `audience` | `optional NotificationAudience` | Original normalized selector union. |
| `created_by_user_id` | `optional string` | Authenticated user that created the Notification. |
| `created_by_asset_id` | `optional int32` | Authenticated device asset that created the Notification. |
| `created_by_app_id` | `optional string` | Trusted app identity stamped by the Wendy agent. |

## Cloud API methods

### `CreateNotification` *(legacy)*

```
CreateNotification(CreateNotificationRequest) → Notification
```

Creates a Notification for one user. This RPC and `CreateNotificationRequest`
are deprecated in their protobuf descriptors and remain intact for existing
Dashboard and legacy clients. `wendycloud.v2` is no longer hypothetical — it is
vendored at `Proto/wendycloud/v2/` and generated into `go/proto/gen/cloudpb/v2/`
— and this RPC is still carried there, still marked `deprecated`. The migration
marker records that it is to be removed from v2, not that it already has been.

---

### `CreateNotificationV2`

```
CreateNotificationV2(CreateNotificationV2Request) → CreateNotificationV2Response
```

Claims the caller-chosen canonical UUID and creates one Notification resource for
the resolved recipient union. The first canonical UUID use may succeed; every
later use returns `ALREADY_EXISTS`, regardless of whether request content is
identical or changed. Cloud does not replay the original response or send pushes
a second time.

User-authenticated callers provide `organization_id`. Provisioned-device callers
omit it because Cloud derives the organization and device from their certificate;
the Wendy agent stamps `app_id` from trusted container state.

#### `CreateNotificationV2Request`

| Field | Type | Description |
|---|---|---|
| `organization_id` | `optional int32` | Required for user-authenticated callers; omitted by provisioned devices. |
| `audience` | `NotificationAudience` | Union of repeated `user_ids`, UUID `team_ids`, and `roles`; see below. |
| `title` | `string` | Notification title. |
| `body` | `string` | Notification body. |
| `severity` | `NotificationSeverity` | `INFO`, `WARNING`, `ERROR`, or `CRITICAL`. |
| `deep_link` | `string` | Absolute `wendy://` URI. |
| `notification_id` | `string` | Caller-chosen Notification resource UUID v4; Cloud stores and returns canonical lowercase form and rejects every canonical reuse with `ALREADY_EXISTS`. |
| `metadata` | `optional Struct` | Structured JSON-compatible metadata. |
| `app_id` | `optional string` | Required for provisioned-device calls and stamped from trusted app identity by the Wendy agent. |

For ACME-enrolled device callers, Cloud v2 `audience.team_ids` is a
`repeated string` containing the canonical UUIDs supplied to the app-facing
`team_uuids` field. Legacy numeric `team_ids` are forwarded only through the
Cloud v1 path. The app-facing Agent API rejects requests that combine numeric
`team_ids` and UUID `team_uuids`.

#### `CreateNotificationV2Response`

| Field | Type | Description |
|---|---|---|
| `notification_id` | `string` | Canonical lowercase UUID of the newly created Notification resource. |

Recipient totals are intentionally omitted because team and role counts can disclose
organization membership.

---

### `ListNotifications` *(server-streaming)*

```
ListNotifications(ListNotificationsRequest) → stream ListNotificationsResponse
```

Returns one Notification per streamed message.

#### `ListNotificationsRequest`

| Field | Type | Description |
|---|---|---|
| `organization_id` | `int32` | Organization to query. |
| `user_id` | `string` | User whose Notifications are requested. |
| `offset` | `optional int32` | Number of matching Notifications to skip. |
| `limit` | `optional int32` | Maximum number of Notifications to return. |
| `severity_filter` | `optional NotificationSeverity` | Restrict results by severity. |
| `include_deleted` | `bool` | Include soft-deleted Notifications. |

#### `ListNotificationsResponse`

| Field | Type | Description |
|---|---|---|
| `notification` | `Notification` | Notification carried by this stream message. |
| `total` | `int32` | Total number of matching Notifications. |

> **Migration note:** `ListNotifications` previously returned one unary response
> containing `repeated notifications`, `next_page_token`, and `total_count`.
> Clients must now receive the server stream until it closes and use
> `offset`/`limit` instead of `page_size`/`page_token`.

---

### `GetNotification`

```
GetNotification(GetNotificationRequest) → Notification
```

Returns one Notification by ID.

### `DeleteNotification`

```
DeleteNotification(DeleteNotificationRequest) → DeleteNotificationResponse
```

Deletes one Notification by ID.

### `MarkAsRead`

```
MarkAsRead(MarkAsReadRequest) → MarkAsReadResponse
```

Marks the requested Notification IDs as read and returns the number marked.
