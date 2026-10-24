#include "_cgo_export.h"

#if defined(_WIN32)
#define HOST_EXPORT __declspec(dllexport)
#else
#define HOST_EXPORT
#endif

HOST_EXPORT WendyHost WENDY_CORE_CALL WendyHostCreate(WendyHostCallbacks const *cb) {
    return wendyHostCreate((WendyHostCallbacks *)cb);
}
HOST_EXPORT void WENDY_CORE_CALL WendyHostRelease(WendyHost host) {
    wendyHostRelease(host);
}
HOST_EXPORT void WENDY_CORE_CALL WendyHostComplete(WendyHostCall call, WendyHostResult const *result) {
    wendyHostComplete(call, (WendyHostResult *)result);
}
HOST_EXPORT int32_t WENDY_CORE_CALL WendyHostStreamHeader(WendyHostCall call, WendyHostMetadata metadata) {
    return wendyHostStreamHeader(call, metadata);
}
HOST_EXPORT int32_t WENDY_CORE_CALL WendyHostStreamMessage(WendyHostCall call, WendyHostBytes message) {
    return wendyHostStreamMessage(call, message);
}
