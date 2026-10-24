#ifndef WENDY_PLATFORM_H
#define WENDY_PLATFORM_H

#include "WendyCore.h"
#include <stddef.h>

#ifdef __cplusplus
extern "C" {
#endif

/**
 * @file
 * Host signing and serialized RPC capabilities (0.1.x, experimental).
 *
 * Handles are opaque process-local integers, never pointers. Zero is invalid.
 * All byte views and arrays are borrowed, immutable and valid only until the
 * receiving function/callback returns; receivers copy anything needed later.
 * NULL is allowed for an empty view/array. Strings are UTF-8 byte views without
 * a terminator. Metadata preserves order, repeated names and raw -bin values;
 * names use lowercase gRPC spelling. Hosts validate names/values.
 *
 * Callbacks may run on any Core thread and concurrently for different calls.
 * For one call, start, request_next and cancel are serialized. They must return
 * promptly, schedule asynchronous work and never wait for Core. Completion may
 * be inline/reentrant or on any host thread. No Go pointer crosses this ABI.
 * This lower provider boundary has its own callback rules, distinct from the
 * upper binding operation callbacks.
 */
typedef uint64_t WendyHostCall;

typedef struct WendyHostBytes {
    uint8_t const *data;
    size_t size;
} WendyHostBytes;

typedef struct WendyHostMetadataEntry {
    WendyHostBytes name;
    WendyHostBytes value;
} WendyHostMetadataEntry;

typedef struct WendyHostMetadata {
    WendyHostMetadataEntry const *entries;
    size_t count;
} WendyHostMetadata;

/** Operation codes: sign exact bytes, fetch leaf-first DER certificate chain,
 * fetch DER SubjectPublicKeyInfo, unary RPC, open server-streaming RPC. */
enum {
    WendyHostSign = 1,
    WendyHostCertificateChain = 2,
    WendyHostPublicKey = 3,
    WendyHostUnary = 4,
    WendyHostServerStream = 5,
    /** Host secure entropy: request.message is a 4-byte unsigned big-endian
     * byte count (1..1048576). Success returns exactly that many unpredictable
     * bytes in result.message. Used for DPoP/nonces/correlation IDs; no keys.
     * Existing hosts may return UNIMPLEMENTED until they support this operation.
     * It follows the same completion/cancellation rules as signing. */
    WendyHostRandomBytes = 6
};

typedef struct WendyHostRequest {
    uint32_t operation;
    WendyHostBytes message;
    WendyHostBytes method; /**< /package.Service/Method; empty for signing. */
    WendyHostMetadata metadata;
    int64_t deadline_unix_millis; /**< UTC Unix milliseconds; 0 = no deadline. */
} WendyHostRequest;

/** A unary/signing result or terminal stream status. Code is the numeric gRPC
 * status (0 = OK); unknown codes are preserved. On failure, message is empty and
 * status_message contains the original diagnostic (never private key material
 * or credential-bearing payloads). Initial/trailing metadata survive errors.
 * For certificate-chain success, certificates is the leaf-first DER array;
 * for sign/public-key/unary success, message is the raw result (possibly empty).
 * A successful stream terminal has no message; deliver items separately. */
typedef struct WendyHostResult {
    uint32_t code;
    WendyHostBytes status_message;
    WendyHostBytes message;
    WendyHostBytes const *certificates;
    size_t certificate_count;
    WendyHostMetadata initial_metadata;
    WendyHostMetadata trailing_metadata;
} WendyHostResult;

typedef struct WendyHostCallbacks {
    void *context; /**< Host-owned; live until release(context) returns. */
    WendyHostBytes algorithm; /**< Copied at Create; e.g. "ML-DSA-65". */
    void (WENDY_CORE_CALL *start)(void *context, WendyHostCall call,
                                 WendyHostRequest const *request);
    void (WENDY_CORE_CALL *request_next)(void *context, WendyHostCall call);
    void (WENDY_CORE_CALL *cancel)(void *context, WendyHostCall call);
    void (WENDY_CORE_CALL *release)(void *context); /**< Optional. */
} WendyHostCallbacks;

/** Copies the table/algorithm; returns one owned registration, or 0 for an
 * invalid table (start, request_next, cancel and a nonempty algorithm required).
 * Host owns connections, TLS/authentication and private keys; Core never retries
 * an application RPC or signs anything beyond the exact supplied message. A
 * registration represents one key/algorithm; certificate renewal keeps the key.
 * At least the public key or a certificate chain must be supplied.
 * On failure no ownership is transferred and release is not called. */
WendyHost WENDY_CORE_CALL WendyHostCreate(WendyHostCallbacks const *callbacks);

/** Removes a registration and cancels unfinished calls. Idempotent; accepts 0.
 * Returns promptly even from a callback. release(context) runs exactly once,
 * possibly before this returns, after all Core callbacks have returned. No
 * later Core callback will use context. Host must arrange its own outstanding
 * asynchronous work's lifetime; it may complete late using only the call ID.
 * Retained Go adapters subsequently return context.Canceled. */
void WENDY_CORE_CALL WendyHostRelease(WendyHost host);

/** Copies a result and terminates a call once. Duplicate/late/unknown handles
 * are safely ignored without reading result. For streams, terminal metadata is
 * retained and the final already-accepted item is delivered before the status.
 * If headers were sent, those remain authoritative; otherwise initial_metadata
 * supplies them. Cancellation/deadline statuses map to native context errors. */
void WENDY_CORE_CALL WendyHostComplete(WendyHostCall call,
                                      WendyHostResult const *result);

/** Supplies stream headers once (including an empty set), before any item.
 * Returns 1 if accepted, 0 if already sent or the call is not a live stream. */
int32_t WENDY_CORE_CALL WendyHostStreamHeader(WendyHostCall call,
                                            WendyHostMetadata metadata);

/** Pushes one serialized message, including an empty message, per request_next.
 * A stream starts with zero demand. Returns 1 if accepted, 0 if there is no
 * demand, no headers yet, or the call has ended. Host must serialize its item
 * and terminal deliveries to preserve order. No unbounded queue is created.
 * Cancel is sent once for locally canceled unfinished calls, after start
 * returns. After cancel no further request_next is made; late deliveries are
 * ignored. Host should cancel its work and dispose of call resources, without
 * needing to complete. Normal terminal completion also disposes host work. */
int32_t WENDY_CORE_CALL WendyHostStreamMessage(WendyHostCall call,
                                             WendyHostBytes message);

#ifdef __cplusplus
}
#endif
#endif
