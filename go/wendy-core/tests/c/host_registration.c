/* Independently linked ABI/lifecycle check, also built on Windows. */
#include <WendyCore/WendyPlatform.h>
#include <assert.h>

static void WENDY_CORE_CALL start(void *context, WendyHostCall call,
                                 WendyHostRequest const *request) {
    (void)context; (void)call; (void)request;
    assert(0); /* no domain operation is issued by this smoke test */
}
static void WENDY_CORE_CALL next(void *context, WendyHostCall call) {
    (void)context; (void)call;
    assert(0);
}
static void WENDY_CORE_CALL cancel(void *context, WendyHostCall call) {
    (void)context; (void)call;
    assert(0);
}
static void WENDY_CORE_CALL release(void *context) { ++*(int *)context; }

int main(void) {
    int released = 0;
    uint8_t const algorithm[] = "ML-DSA-65";
    WendyHostCallbacks cb = { &released, {algorithm, sizeof algorithm - 1},
                              start, next, cancel, release };
    assert(WendyHostCreate(NULL) == 0);
    WendyHost host = WendyHostCreate(&cb);
    assert(host != 0);
    WendyHostRelease(host);
    WendyHostRelease(host);
    assert(released == 1);
    WendyHostComplete(0, NULL);
    assert(WendyHostStreamHeader(0, (WendyHostMetadata){0}) == 0);
    assert(WendyHostStreamMessage(0, (WendyHostBytes){0}) == 0);
    return 0;
}
