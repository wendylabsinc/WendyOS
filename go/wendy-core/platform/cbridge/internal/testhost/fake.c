#include "fake.h"
#include <stdatomic.h>
#include <stdlib.h>
#include <string.h>

struct FakeHost {
    WendyHost id;
    int mode;
    _Atomic uint64_t call, starts, nexts, cancels, released, invalid;
    _Atomic uint64_t operation, deadline;
    _Atomic uint64_t entropy, signs, streams;
    WendyHostBytes public_key, first, second, proof, request1, request2;
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

static WendyHostBytes copy_view(WendyHostBytes b) {
    uint8_t *p = malloc(b.size ? b.size : 1);
    if (b.size) memcpy(p, b.data, b.size);
    return view(p, b.size);
}

static void cloud_start(FakeHost *h, WendyHostCall call, WendyHostRequest const *r) {
    WendyHostResult result = {0};
    if (r->operation == WendyHostRandomBytes) {
        atomic_fetch_add(&h->entropy, 1);
        if (h->mode == 10) return;
        if (r->message.size != 4) { atomic_fetch_add(&h->invalid, 1); return; }
        uint32_t n = 0;
        for (int i = 0; i < 4; i++) n = (n << 8) | r->message.data[i];
        if (n > 1048576) abort();
        uint8_t *bytes = malloc(n);
        memset(bytes, (int)atomic_load(&h->entropy), n); /* deterministic test entropy */
        result.message = view(bytes, n);
        WendyHostComplete(call, &result);
        memset(bytes, 0, n);
        free(bytes);
        return;
    }
    if (r->operation == WendyHostPublicKey) {
        result.message = h->public_key;
    } else if (r->operation == WendyHostSign) {
        atomic_fetch_add(&h->signs, 1);
        result.message = text("test signature");
    } else if (r->operation == WendyHostServerStream) {
        uint64_t page = atomic_fetch_add(&h->streams, 1);
        if (r->method.size != strlen("/wendycloud.v2.AssetService/ListAssets") ||
            memcmp(r->method.data, "/wendycloud.v2.AssetService/ListAssets", r->method.size))
            atomic_fetch_add(&h->invalid, 1);
        if (page > 1) { atomic_fetch_add(&h->invalid, 1); return; }
        WendyHostBytes *request = page ? &h->request2 : &h->request1;
        *request = copy_view(r->message);
        int auth = 0, dpop = 0, correlation = 0;
        for (size_t i = 0; i < r->metadata.count; i++) {
            WendyHostMetadataEntry e = r->metadata.entries[i];
            if (e.name.size == 13 && !memcmp(e.name.data, "authorization", 13))
                auth = e.value.size == 17 && !memcmp(e.value.data, "DPoP access-token", 17);
            if (e.name.size == 4 && !memcmp(e.name.data, "dpop", 4)) {
                dpop = e.value.size != 0;
                free((void *)h->proof.data);
                h->proof = copy_view(e.value);
            }
            if (e.name.size == 16 && !memcmp(e.name.data, "x-correlation-id", 16))
                correlation = e.value.size == 36;
        }
        if (!auth || !dpop || !correlation) atomic_fetch_add(&h->invalid, 1);
        if (h->mode == 9) return;
        fake_header(call);
        if (h->mode == 11) fake_complete(call, 0); /* empty list */
        return;
    } else {
        atomic_fetch_add(&h->invalid, 1);
        result.code = 12;
    }
    WendyHostComplete(call, &result);
}

static void WENDY_CORE_CALL start(void *context, WendyHostCall call, WendyHostRequest const *r) {
    FakeHost *h = context;
    atomic_fetch_add(&h->starts, 1);
    atomic_store(&h->operation, r->operation);
    atomic_store(&h->deadline, r->deadline_unix_millis);
    atomic_store(&h->call, call);
    if (h->mode >= 7) { cloud_start(h, call, r); return; }
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
    if (h->mode >= 7) {
        if (h->mode == 9) return;
        WendyHostBytes response = atomic_load(&h->streams) == 1 ? h->first : h->second;
        if (WendyHostStreamMessage(call, response) != 1) atomic_fetch_add(&h->invalid, 1);
        fake_complete(call, h->mode == 8 ? 7 : h->mode == 12 ? 123 : 0);
        return;
    }
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
void fake_free(FakeHost *h) {
    free((void *)h->public_key.data);
    free((void *)h->first.data);
    free((void *)h->second.data);
    free((void *)h->proof.data);
    free((void *)h->request1.data);
    free((void *)h->request2.data);
    free(h);
}
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
    case 8: return atomic_load(&h->entropy);
    case 9: return atomic_load(&h->signs);
    case 10: return atomic_load(&h->streams);
    }
    return 0;
}

void fake_cloud_configure(FakeHost *h, WendyHostBytes key, WendyHostBytes first, WendyHostBytes second) {
    h->public_key = copy_view(key);
    h->first = copy_view(first);
    h->second = copy_view(second);
}
WendyHostBytes fake_cloud_view(FakeHost *h, int field) {
    switch (field) {
    case 0: return h->proof;
    case 1: return h->request1;
    case 2: return h->request2;
    }
    return (WendyHostBytes){0};
}

void fake_stale_complete(WendyHostCall call) {
    WendyHostComplete(call, (WendyHostResult const *)(uintptr_t)1);
}

struct FakeCompletion {
    WendyOperation *operation;
    WendyData *response;
    WendyError *error;
    _Atomic uint64_t completed;
    int release_in_callback;
};
static void WENDY_CORE_CALL cloud_complete(void *context, WendyData *data, WendyError *error) {
    FakeCompletion *c = context;
    c->response = WendyDataRetain(data);
    c->error = WendyErrorRetain(error);
    WendyDataRelease(data);
    WendyErrorRelease(error);
    if (c->release_in_callback) {
        WendyOperationCancel(c->operation);
        WendyOperationRelease(c->operation);
        c->operation = NULL;
    }
    atomic_fetch_add(&c->completed, 1);
}
FakeCompletion *fake_cloud_create(WendyHost host, WendyDataView request) {
    FakeCompletion *c = calloc(1, sizeof *c);
    c->operation = WendyCloudListDevicesCreate(host, request, cloud_complete, c);
    return c;
}
void fake_cloud_start(FakeCompletion *c) { WendyOperationStart(c->operation); }
void fake_cloud_cancel(FakeCompletion *c) { WendyOperationCancel(c->operation); }
void fake_cloud_release_operation(FakeCompletion *c) {
    WendyOperationRelease(c->operation); c->operation = NULL;
}
void fake_cloud_release_in_callback(FakeCompletion *c) { c->release_in_callback = 1; }
void fake_cloud_free(FakeCompletion *c) {
    WendyOperationRelease(c->operation);
    WendyDataRelease(c->response);
    WendyErrorRelease(c->error);
    free(c);
}
uint64_t fake_cloud_stat(FakeCompletion *c, int field) {
    switch (field) {
    case 0: return atomic_load(&c->completed);
    case 1: return WendyErrorGetCode(c->error);
    case 2: return c->response != NULL;
    case 3: return c->error != NULL;
    case 4: return c->operation != NULL;
    }
    return 0;
}
WendyDataView fake_cloud_result(FakeCompletion *c, int field) {
    switch (field) {
    case 0: return WendyDataGetBytes(c->response);
    case 1: return WendyErrorGetMessage(c->error);
    case 2: return WendyErrorGetDetails(c->error);
    }
    return (WendyDataView){0};
}
int fake_cloud_invalid_create(void) {
    WendyDataView invalid = {NULL, 1};
    WendyDataView huge = {(void const *)(uintptr_t)1, 1048577};
    if (WendyCloudListDevicesCreate(0, (WendyDataView){0}, NULL, NULL)) return 0;
    if (WendyCloudListDevicesCreate(0, invalid, cloud_complete, NULL)) return 0;
    if (WendyCloudListDevicesCreate(0, huge, cloud_complete, NULL)) return 0;
    WendyOperationStart(NULL);
    WendyOperationCancel(NULL);
    WendyOperationRelease(WendyOperationRetain(NULL));
    WendyDataRelease(WendyDataRetain(NULL));
    WendyErrorRelease(WendyErrorRetain(NULL));
    return WendyDataGetBytes(NULL).size == 0 && WendyErrorGetCode(NULL) == 0 &&
        WendyErrorGetMessage(NULL).size == 0 && WendyErrorGetDetails(NULL).size == 0;
}
