# Assets API

The `AssetService` gRPC service manages the inventory of assets (devices, peripherals, etc.) registered in a Wendy Cloud organization.

## Proto package

`wendycloud.v1` — defined in `Proto/cloud/assets.proto`. **The method reference
below documents v1; the separate v2 notes describe device-binding recovery.**
A parallel `wendycloud.v2.AssetService`
(`Proto/wendycloud/v2/assets.proto`) exists, identifies assets and organizations
by **string UUID** rather than `int32`, and adds `ListAssetsByApp` and
`FilterAssets`. The CLI's OIDC path already calls v2 exclusively; legacy
sessions still use the v1 service described here.

## v2 device-binding and deletion evidence

The following applies to `wendycloud.v2.AssetService` only; the methods below
continue to describe numeric v1. Compatible v2 consumers can resolve the exact
enrollment and recover deletion evidence without a local CLI journal.

### v2 `GetAsset`

| Request field | Type | Meaning |
|---|---|---|
| `id` | `string` | Asset UUID; may be empty when both selectors below are supplied. |
| `organization_id` | `optional string` | Exact tenant UUID; required with `device_id` when `id` is empty. |
| `device_id` | `optional string` | Exact PKI device UUID; required with `organization_id` when `id` is empty. |

An authorized active binding returns `Asset`. An authorized retained deletion
returns `NOT_FOUND` with one typed `DeletedAsset` detail. Plain `NOT_FOUND`, wrong
status, duplicate details, malformed UUID/timestamp or a changed binding are
**not deletion proof**. These typed details are response data, not a separately
signed Cloud tombstone artifact.

| `DeletedAsset` field | Type | Meaning |
|---|---|---|
| `id` | `string` | Deleted asset UUID. |
| `organization_id` | `string` | Owning tenant UUID. |
| `device_id` | `string` | Device UUID bound to this asset. |
| `deleted_at` | `google.protobuf.Timestamp` | Recorded deletion time. |

### v2 `DeleteAsset`

The optional string `expected_device_id` is a compare-and-delete guard: it must
match the exact enrolled device binding when supplied. The Cloud-owned device
unenrollment consumer supplies it rather than deleting an unchecked asset UUID.
Cloud authorizes `device:delete`, revokes the exact device principal through PKI,
then retains a tombstone and deletion audit. A failed revocation does not delete
the active asset. Replayed deletion preserves the original actor/time/evidence.
This operation alone is not proof that the device erased its local credentials.

### v2 reported hardware facts

Optional `Asset` fields `soc_compatible` (36), `serial_number` (37),
`kernel_version` (38), `l4t_version` (39), and `gpu_arch` (40) describe reported
hardware/software facts. Missing reports leave them absent. They are not device
identity, permissions or authorization evidence.

## Methods

These method descriptions apply to numeric v1.

### `CreateAsset`

```
CreateAsset(CreateAssetRequest) → Asset
```

Creates a new asset in the organization.

---

### `GetAsset`

```
GetAsset(GetAssetRequest) → Asset
```

Returns a single asset by ID.

---

### `UpdateAsset`

```
UpdateAsset(UpdateAssetRequest) → Asset
```

Updates mutable fields of an existing asset.

---

### `DeleteAsset`

```
DeleteAsset(DeleteAssetRequest) → DeleteAssetResponse
```

Deletes an asset by ID.

---

### `ListAssets` *(server-streaming)*

```
ListAssets(ListAssetsRequest) → stream ListAssetsResponse
```

Returns a stream of assets matching the request filters. Each streamed message contains one asset. The stream closes when all matching assets have been sent.

#### `ListAssetsRequest`

| Field | Type | Description |
|-------|------|-------------|
| `organization_id` | `int32` | *(required)* Organization to query. |
| `is_compute_device` | `optional bool` | When `true`, restricts results to compute devices. |
| `offset` | `optional int32` | Number of assets to skip (for manual pagination). |
| `limit` | `optional int32` | Maximum number of assets to return. |
| `filter` | `optional string` | Filter expression matching on name, details, asset_type, device_type, or tags. |
| `online_only` | `optional bool` | When `true`, only assets with an active broker presence are returned. |

#### `ListAssetsResponse` *(one message per streamed asset)*

| Field | Type | Description |
|-------|------|-------------|
| `asset` | `Asset` | The asset for this stream message. |
| `total` | `int32` | Total number of matching assets (may be sent on the first or last message). |

---

### `ListAssetChildren` *(server-streaming)*

```
ListAssetChildren(ListAssetChildrenRequest) → stream ListAssetChildrenResponse
```

Returns a stream of direct children of a given parent asset.

#### `ListAssetChildrenRequest`

| Field | Type | Description |
|-------|------|-------------|
| `parent_asset_id` | `int32` | *(required)* Parent asset ID. |
| `offset` | `optional int32` | Number of assets to skip. |
| `limit` | `optional int32` | Maximum number of assets to return. |

#### `ListAssetChildrenResponse` *(one message per streamed asset)*

| Field | Type | Description |
|-------|------|-------------|
| `asset` | `Asset` | The child asset for this stream message. |
| `total` | `int32` | Total number of matching children. |

---

### `GetAssetLineage`

```
GetAssetLineage(GetAssetLineageRequest) → GetAssetLineageResponse
```

Returns the full asset lineage (root asset, all intermediate assets) for a given asset ID.

## Streaming behaviour

`ListAssets` and `ListAssetChildren` are **server-streaming** RPCs. Clients must open a stream and call `Recv()` in a loop until `io.EOF` is returned:

```go
stream, err := client.ListAssets(ctx, req)
if err != nil {
    return err
}
var assets []*cloudpb.Asset
for {
    resp, err := stream.Recv()
    if err == io.EOF {
        break
    }
    if err != nil {
        return err
    }
    assets = append(assets, resp.GetAsset())
}
```

> **Migration note:** Prior to PR #601 `ListAssets` and `ListAssetChildren` were unary RPCs that returned a paginated response (`Assets []Asset`, `NextPageToken string`). They are now server-streaming. The old `page_size` / `page_token` fields have been replaced by `offset` / `limit`. Update any existing client code accordingly.

## Online-only filtering

Pass `online_only = true` to receive only devices with an active broker presence. The `wendy cloud discover` and `wendy cloud tunnel` commands both use this filter by default; `wendy cloud discover --all` omits it to include offline devices.
