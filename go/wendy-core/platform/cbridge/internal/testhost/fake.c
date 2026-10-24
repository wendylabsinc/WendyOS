#include "fake.h"
#include <stdatomic.h>
#include <stdlib.h>
#include <string.h>

struct FakeHost {
    WendyHost id;
    int mode;
    _Atomic uint64_t call, starts, nexts, cancels, released, invalid;
    _Atomic uint64_t operation, deadline;
};

static WendyHostBytes view(void const *p, size_t n) {
    WendyHostBytes b = {p, n};
    return b;
}
static WendyHostBytes text(char const *s) { return view(s, strlen(s)); }

int fake_header(WendyHostCall call) {
    uint8_t binary[] = {0, 255, 0};
    WendyHostMetadataEntry entries[] = {
        {text("trace-bin"), view(binary, sizeof binary)},
        {text("trace-bin"), text("second")}
    };
    WendyHostMetadata md = {entries, 2};
    return WendyHostStreamHeader(call, md);
}

int fake_push(WendyHostCall call) {
    uint8_t binary[] = {0, 255, 0};
    return WendyHostStreamMessage(call, view(binary, sizeof binary));
}

void fake_complete(WendyHostCall call, uint32_t code) {
    uint8_t binary[] = {0, 255, 0};
    WendyHostMetadataEntry entries[] = {
        {text("details-bin"), view(binary, sizeof binary)},
        {text("details-bin"), text("second")}
    };
    WendyHostResult r = {0};
    r.code = code;
    r.status_message = text("private remote diagnostic");
    r.message = view(binary, sizeof binary);
    r.initial_metadata = (WendyHostMetadata){entries, 2};
    r.trailing_metadata = (WendyHostMetadata){entries, 2};
    WendyHostComplete(call, &r);
    memset(binary, 0, sizeof binary); /* completion must copy */
}

static void WENDY_CORE_CALL start(void *context, WendyHostCall call, WendyHostRequest const *r) {
    FakeHost *h = context;
    atomic_fetch_add(&h->starts, 1);
    atomic_store(&h->operation, r->operation);
    atomic_store(&h->deadline, r->deadline_unix_millis);
    atomic_store(&h->call, call);
    if (h->mode == 6) { WendyHostRelease(h->id); return; }
    if (h->mode == 2 || h->mode == 5) return;
    if (h->mode == 1) { fake_complete(call, 7); return; }
    if (r->operation == WendyHostUnary && r->method.size &&
        (r->method.size != strlen("/test.Service/Unary") ||
         memcmp(r->method.data, "/test.Service/Unary", r->method.size)))
        atomic_fetch_add(&h->invalid, 1);
    if (r->operation == WendyHostServerStream) {
        if (fake_push(call) != 0) atomic_fetch_add(&h->invalid, 1);
        if (fake_header(call) != 1) atomic_fetch_add(&h->invalid, 1);
        if (fake_header(call) != 0) atomic_fetch_add(&h->invalid, 1);
        if (fake_push(call) != 0) atomic_fetch_add(&h->invalid, 1);
        return;
    }
    uint8_t key[] = {3, 0, 4};
    WendyHostBytes certs[] = {text("leaf"), text("issuer")};
    WendyHostResult result = {0};
    switch (r->operation) {
    case WendyHostSign:
    case WendyHostUnary:
        result.message = r->message;
        result.initial_metadata = r->metadata;
        break;
    case WendyHostPublicKey:
        result.message = view(key, sizeof key);
        break;
    case WendyHostCertificateChain:
        result.certificates = certs;
        result.certificate_count = 2;
        break;
    default:
        atomic_fetch_add(&h->invalid, 1);
    }
    WendyHostComplete(call, &result);
    WendyHostComplete(call, NULL); /* duplicate must not replace success */
    memset(key, 0, sizeof key);
}

static void WENDY_CORE_CALL next(void *context, WendyHostCall call) {
    FakeHost *h = context;
    uint64_t n = atomic_fetch_add(&h->nexts, 1);
    if (h->mode == 5) return;
    if (n == 0) {
        if (WendyHostStreamMessage(call, (WendyHostBytes){0}) != 1) atomic_fetch_add(&h->invalid, 1);
        if (fake_push(call) != 0) atomic_fetch_add(&h->invalid, 1);
    } else if (n == 1 && h->mode != 4) {
        if (fake_push(call) != 1) atomic_fetch_add(&h->invalid, 1);
        /* End inline, while the accepted item is still buffered. */
        fake_complete(call, 0);
    } else {
        fake_complete(call, 16);
    }
}

static void WENDY_CORE_CALL cancel(void *context, WendyHostCall call) {
    FakeHost *h = context;
    atomic_fetch_add(&h->cancels, 1);
    fake_complete(call, 0); /* completion after cancel must be harmless */
}

static void WENDY_CORE_CALL release(void *context) {
    FakeHost *h = context;
    atomic_fetch_add(&h->released, 1);
}

FakeHost *fake_create(int mode) {
    FakeHost *h = calloc(1, sizeof *h);
    h->mode = mode;
    WendyHostCallbacks callbacks = {h, text("ML-DSA-65"), start, next, cancel, release};
    h->id = WendyHostCreate(&callbacks);
    return h;
}
void fake_free(FakeHost *h) { free(h); }
WendyHost fake_id(FakeHost *h) { return h->id; }
uint64_t fake_stat(FakeHost *h, int field) {
    switch (field) {
    case 0: return atomic_load(&h->call);
    case 1: return atomic_load(&h->starts);
    case 2: return atomic_load(&h->nexts);
    case 3: return atomic_load(&h->cancels);
    case 4: return atomic_load(&h->released);
    case 5: return atomic_load(&h->invalid);
    case 6: return atomic_load(&h->operation);
    case 7: return atomic_load(&h->deadline);
    }
    return 0;
}

void fake_stale_complete(WendyHostCall call) {
    WendyHostComplete(call, (WendyHostResult const *)(uintptr_t)1);
}
