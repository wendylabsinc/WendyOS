#include "objects.h"
#include "_cgo_export.h"
#include <stdatomic.h>
#include <string.h>

#if defined(_WIN32)
#define CORE_EXPORT __declspec(dllexport)
#else
#define CORE_EXPORT
#endif

/* Only C allocations cross the ABI. A running operation holds a reference,
 * independently of the caller, until its completion callback has returned. */
struct WendyOperation { _Atomic uint32_t refs; uint64_t id; };
struct WendyData { _Atomic uint32_t refs; WendyDataView bytes; };
struct WendyError { _Atomic uint32_t refs; uint32_t code; WendyData *message, *details; };

WendyOperation *operation_new(uint64_t id) {
    WendyOperation *o = malloc(sizeof *o);
    if (o) { atomic_init(&o->refs, 1); o->id = id; }
    return o;
}
uint64_t operation_id(WendyOperation const *o) { return o ? o->id : 0; }
CORE_EXPORT WendyOperation *WENDY_CORE_CALL WendyOperationRetain(WendyOperation *o) {
    if (o) atomic_fetch_add_explicit(&o->refs, 1, memory_order_relaxed);
    return o;
}
CORE_EXPORT void WENDY_CORE_CALL WendyOperationRelease(WendyOperation *o) {
    if (o && atomic_fetch_sub_explicit(&o->refs, 1, memory_order_acq_rel) == 1) {
        wendyOperationDispose(o->id);
        free(o);
    }
}
CORE_EXPORT void WENDY_CORE_CALL WendyOperationStart(WendyOperation *o) {
    if (o) wendyOperationStart(o->id, o);
}
CORE_EXPORT void WENDY_CORE_CALL WendyOperationCancel(WendyOperation *o) {
    if (o) wendyOperationCancel(o->id);
}
CORE_EXPORT WendyOperation *WENDY_CORE_CALL WendyCloudListDevicesCreate(
    WendyHost host, WendyDataView request, WendyCompletion completion, void *context) {
    return wendyCloudListDevicesCreate(host, request, completion, context);
}
void operation_complete(WendyCompletion completion, void *context, WendyData *data, WendyError *error) {
    completion(context, data, error);
}

WendyData *data_new(WendyDataView bytes) {
    WendyData *d = calloc(1, sizeof *d);
    if (!d) abort(); /* Same out-of-memory policy as the Go/cgo allocator. */
    atomic_init(&d->refs, 1);
    if (bytes.size) {
        uint8_t *p = malloc(bytes.size);
        if (!p) abort();
        memcpy(p, bytes.data, bytes.size);
        d->bytes = (WendyDataView){p, bytes.size};
    }
    return d;
}
CORE_EXPORT WendyData *WENDY_CORE_CALL WendyDataRetain(WendyData *d) {
    if (d) atomic_fetch_add_explicit(&d->refs, 1, memory_order_relaxed);
    return d;
}
CORE_EXPORT void WENDY_CORE_CALL WendyDataRelease(WendyData *d) {
    if (d && atomic_fetch_sub_explicit(&d->refs, 1, memory_order_acq_rel) == 1) {
        free((void *)d->bytes.data);
        free(d);
    }
}
CORE_EXPORT WendyDataView WENDY_CORE_CALL WendyDataGetBytes(WendyData const *d) {
    return d ? d->bytes : (WendyDataView){0};
}
WendyError *error_new(uint32_t code, WendyDataView message, WendyDataView details) {
    WendyError *e = malloc(sizeof *e);
    if (!e) abort();
    atomic_init(&e->refs, 1);
    e->code = code;
    e->message = data_new(message);
    e->details = data_new(details);
    return e;
}
CORE_EXPORT WendyError *WENDY_CORE_CALL WendyErrorRetain(WendyError *e) {
    if (e) atomic_fetch_add_explicit(&e->refs, 1, memory_order_relaxed);
    return e;
}
CORE_EXPORT void WENDY_CORE_CALL WendyErrorRelease(WendyError *e) {
    if (e && atomic_fetch_sub_explicit(&e->refs, 1, memory_order_acq_rel) == 1) {
        WendyDataRelease(e->message);
        WendyDataRelease(e->details);
        free(e);
    }
}
CORE_EXPORT uint32_t WENDY_CORE_CALL WendyErrorGetCode(WendyError const *e) { return e ? e->code : 0; }
CORE_EXPORT WendyDataView WENDY_CORE_CALL WendyErrorGetMessage(WendyError const *e) {
    return WendyDataGetBytes(e ? e->message : NULL);
}
CORE_EXPORT WendyDataView WENDY_CORE_CALL WendyErrorGetDetails(WendyError const *e) {
    return WendyDataGetBytes(e ? e->details : NULL);
}
