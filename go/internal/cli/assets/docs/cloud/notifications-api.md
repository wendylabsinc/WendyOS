# Notifications API

The `NotificationService` gRPC service creates, lists, and manages
operator-facing Wendy Notifications in a Wendy Cloud organization.

## Proto package

`wendycloud.v1` — defined in `Proto/cloud/notifications.proto`.

## App-facing API (`wendy.system.v1`)

Apps with the `notifications` entitlement call
`wendy.system.v1.NotificationService` over the Unix socket at
`$WENDY_SYSTEM_SOCKET` (`/run/wendy/system/system.sock`). WendyKit exposes this
as `WendyNotification.send(_:)`, so apps normally do not call gRPC directly.

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

The app-facing and Cloud messages use the same plural selector shape. All three
fields have union semantics. At most 100 selector entries may be supplied across
the three lists. Cloud normalizes and deduplicates them, remains authoritative
for recipient resolution, and resolves at most 10,000 recipients.

| Field | Type | Description |
|---|---|---|
| `user_ids` | `repeated string` | User IDs to include. |
| `team_ids` | `repeated int32` | Organization team IDs to include. |
| `roles` | `repeated OrganizationRole` | Organization roles to include. |

At least one selector is required. A user selected through more than one field
receives one Notification.

#### `SendResponse`

| Field | Type | Description |
|---|---|---|
| `notification_id` | `string` | Canonical lowercase UUID of the newly created Notification resource. |

Recipient totals are intentionally omitted because team and role counts can disclose
organization membership.

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
| `audience` | `NotificationAudience` | Union of repeated `user_ids`, `team_ids`, and `roles`; see above. |
| `title` | `string` | Notification title. |
| `body` | `string` | Notification body. |
| `severity` | `NotificationSeverity` | `INFO`, `WARNING`, `ERROR`, or `CRITICAL`. |
| `deep_link` | `string` | Absolute `wendy://` URI. |
| `notification_id` | `string` | Caller-chosen Notification resource UUID v4; Cloud stores and returns canonical lowercase form and rejects every canonical reuse with `ALREADY_EXISTS`. |
| `metadata` | `optional Struct` | Structured JSON-compatible metadata. |
| `app_id` | `optional string` | Required for provisioned-device calls and stamped from trusted app identity by the Wendy agent. |

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

## Campaign notifications and catalog registration

A campaign with `notify.on: event` and `notify.event: <name>` sends an immediate
notification for the matching event. `notify.on: detection` sends on an inference
appearance. Omit `notify.webhook` for Wendy Cloud delivery, or supply an HTTP(S)
endpoint for webhook delivery. `episode_committed` remains manifest intent for
notification after upload; it is separate from immediate delivery.

The agent uses its enrolled device credentials and a bounded, nonpersistent
queue. There are at most three attempts, with 10-second timeouts and the same
event UUID. `InvalidArgument`, `Unauthenticated`, `PermissionDenied`,
`FailedPrecondition`, `AlreadyExists`, `Unimplemented`, and `DataLoss` stop
retries. A mismatched response notification ID is `DataLoss`. Other failures
are retried with backoff. Errors appear in agent logs and campaign
`inference_status.notification_error`.

### App catalog API

The v1 `AppService` exposes `UpsertApp`, `GetApp`, `UpdateApp`, `DeleteApp`, and
`ListApps`. It replaces the former `CreateApp` RPC; clients must regenerate
against `Proto/cloud/apps.proto`. Mutations and `GetApp` carry both `id` and
`organization_id`. `UpsertApp` accepts optional `name` and `details`; `UpdateApp`
also accepts the owner/admin-controlled `can_send_notifications` grant.
`ListApps` uses `organization_id`, optional `offset`, `limit`, and `filter`,
returning `apps` and `total`. Pagination is offset-based, not page-token-based.
Deployment registration uses GetApp first and UpsertApp only when absent, so it
preserves existing metadata and never automatically grants notifications.
