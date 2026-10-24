#ifndef WENDY_CORE_H
#define WENDY_CORE_H

#include <stdint.h>
#include <stddef.h>

#if defined(_WIN32)
#  define WENDY_CORE_CALL __cdecl
#else
#  define WENDY_CORE_CALL
#endif

#ifdef __cplusplus
extern "C" {
#endif

/**
 * Returns 42 from the shared Go engine.
 *
 * This operation is synchronous, thread-safe, and requires no platform
 * capability. It returns a scalar without transferring resources; runtime
 * initialization is separate.
 * There are no input pointers, owned objects, callbacks, or error conditions.
 *
 * Binding pattern: scalar-unary.
 * Ownership: returns a value; no resources are transferred.
 * Threading: safe for concurrent calls.
 * Availability: 0.0.1.
 */
int32_t WENDY_CORE_CALL WendyHelloWorld(void);

/**
 * Returns the effective Wendy Core version without a leading v.
 *
 * Packaged engines report their build identity, including preview/development
 * suffixes. Unreleased source builds use a language-neutral development
 * identity such as 0.1.0-dev+a0e8ddc when a commit is known, or 0.1.0-dev
 * otherwise. Tagged dependencies retain their release/prerelease version.
 * The NUL-terminated UTF-8 string is borrowed and valid for the life of the
 * process. Callers must not modify or free it. No Go pointer crosses
 * the ABI, and there are no error conditions.
 *
 * Binding pattern: borrowed-string-unary.
 * Ownership: returns a borrowed, read-only string; no resources are transferred.
 * Threading: safe for concurrent calls.
 * Availability: 0.1.0-seed.1.
 */
char *WENDY_CORE_CALL WendyCoreVersion(void);

/** Process-local host registration; see WendyPlatform.h. Zero is invalid. */
typedef uint64_t WendyHost;
typedef struct WendyOperation WendyOperation;
typedef struct WendyData WendyData;
typedef struct WendyError WendyError;

/** Borrowed immutable bytes. NULL is valid only when size is zero. */
typedef struct WendyDataView {
    uint8_t const *data;
    size_t size;
} WendyDataView;

/** Exactly one of response/error is non-NULL, including for an empty inventory.
 * Transfers one owned reference to that object; release it through Wendy.
 * context remains host-owned and must live through the callback's return.
 * Runs once on a Core thread after activation; it may cancel/retain/release
 * objects, including the operation, and must return promptly. */
typedef void (WENDY_CORE_CALL *WendyCompletion)(void *context,
                                               WendyData *response,
                                               WendyError *error);

/** Creates a dormant Cloud device-list operation. request is a serialized
 * wendy.core.v1.CloudDeviceListRequest; copied before returning. The host must
 * already provide TLS to that authority, a token bound to its key, a validated
 * ML-DSA-65 public key, and WendyHostRandomBytes (see WendyPlatform.h).
 * Core calls core/cloud.Client.ListDevices, builds DPoP/correlation metadata,
 * and delegates entropy, signing and network I/O to that host. No fallback,
 * token refresh or application retry is added. Host release cancels host calls.
 * Success is wendy.core.v1.CloudDeviceListResponse (complete or error, no
 * public offsets). Errors preserve numeric gRPC status and RPCErrorDetails.
 * Returns NULL for a missing completion, invalid byte view, request > 1 MiB or
 * allocation failure; no ownership transfers or callback occurs in that case.
 * Other input errors complete with INVALID_ARGUMENT after activation.
 * Store the returned handle, then call WendyOperationStart exactly once. This
 * guarantees completion cannot precede this initiating function's return.
 * Binding pattern: async-unary.
 * Ownership: returns one owned operation reference; inputs are borrowed.
 * Threading: safe for concurrent calls; completion never runs during Create.
 * Cancellation: WendyOperationCancel; terminal delivery wins races.
 * Availability: 0.1.x experimental.
 */
WendyOperation *WENDY_CORE_CALL WendyCloudListDevicesCreate(
    WendyHost host, WendyDataView request, WendyCompletion completion, void *context);

/** Activates a created operation once; duplicates/NULL are harmless. Call only
 * after Create has returned and the host has stored its handle. Completion may
 * race this function's return, on another Core thread. Starting never waits for
 * host callbacks. An unstarted operation's last Release discards it without a
 * callback. After Start, releasing the caller's reference does not cancel work.
 * Binding pattern: owned-object.
 */
void WENDY_CORE_CALL WendyOperationStart(WendyOperation *operation);
/** Requests cancellation without waiting. Idempotent; accepts NULL. Before
 * Start it records cancellation for delivery after Start. The host receives
 * C1 cancellation for pending calls; late host results are ignored safely.
 * Binding pattern: owned-object.
 */
void WENDY_CORE_CALL WendyOperationCancel(WendyOperation *operation);
/** Adds an owned reference; accepts NULL. Binding pattern: owned-object. */
WendyOperation *WENDY_CORE_CALL WendyOperationRetain(WendyOperation *operation);
/** Drops a reference; accepts NULL. Binding pattern: owned-object. */
void WENDY_CORE_CALL WendyOperationRelease(WendyOperation *operation);

/** Adds an owned reference; accepts NULL. Binding pattern: owned-object. */
WendyData *WENDY_CORE_CALL WendyDataRetain(WendyData *data);
/** Drops a reference; accepts NULL. Binding pattern: owned-object. */
void WENDY_CORE_CALL WendyDataRelease(WendyData *data);
/** Borrows immutable bytes until data's final Release; NULL gives an empty view.
 * Binding pattern: owned-object.
 */
WendyDataView WENDY_CORE_CALL WendyDataGetBytes(WendyData const *data);

/** Adds an owned reference; accepts NULL. Binding pattern: owned-object. */
WendyError *WENDY_CORE_CALL WendyErrorRetain(WendyError *error);
/** Drops a reference; accepts NULL. Binding pattern: owned-object. */
void WENDY_CORE_CALL WendyErrorRelease(WendyError *error);
/** Numeric gRPC code, including unknown codes (NULL gives 0). Local cancellation
 * is 1, deadline is 4, invalid input is 3, other local failures are 13 (INTERNAL).
 * Binding pattern: owned-object.
 */
uint32_t WENDY_CORE_CALL WendyErrorGetCode(WendyError const *error);
/** Borrows a diagnostic UTF-8 message, without a terminator, until final Release.
 * NULL gives an empty view. Binding pattern: owned-object.
 */
WendyDataView WENDY_CORE_CALL WendyErrorGetMessage(WendyError const *error);
/** Borrows wendy.core.v1.RPCErrorDetails protobuf until final Release; NULL gives
 * an empty view. Metadata keeps order/repetition/binary bytes. Remote messages
 * and metadata are untrusted; hosts must keep credential-bearing data out of logs.
 * Binding pattern: owned-object.
 */
WendyDataView WENDY_CORE_CALL WendyErrorGetDetails(WendyError const *error);

#ifdef __cplusplus
}
#endif

#endif
