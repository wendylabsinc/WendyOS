#ifndef WENDY_HOST_CALLBACKS_H
#define WENDY_HOST_CALLBACKS_H
#include "../../include/WendyCore/WendyPlatform.h"
#include <stdlib.h>

static inline void host_start(WendyHostCallbacks cb, WendyHostCall call, WendyHostRequest *r) {
    cb.start(cb.context, call, r);
}
static inline void host_next(WendyHostCallbacks cb, WendyHostCall call) {
    cb.request_next(cb.context, call);
}
static inline void host_cancel(WendyHostCallbacks cb, WendyHostCall call) {
    cb.cancel(cb.context, call);
}
static inline void host_release(WendyHostCallbacks cb) {
    if (cb.release) cb.release(cb.context);
}
static inline int host_valid(WendyHostCallbacks *cb) {
    return cb && cb->start && cb->request_next && cb->cancel &&
           cb->algorithm.data && cb->algorithm.size;
}
#endif
