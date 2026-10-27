#include <node_api.h>
#include "native_loader.h"

static void dispose(napi_env env, void *data, void *hint) {
    (void)env; (void)hint;
    free(data); /* The Go library itself remains loaded. */
}

static napi_value hello(napi_env env, napi_callback_info info) {
    void *data;
    if (napi_get_cb_info(env, info, NULL, NULL, NULL, &data) != napi_ok) return NULL;
    WendyCoreLibrary const *library = (WendyCoreLibrary const *)data;
    napi_value result;
    if (napi_create_int32(env, library->hello(), &result) != napi_ok) return NULL;
    return result;
}

static napi_value version(napi_env env, napi_callback_info info) {
    void *data;
    if (napi_get_cb_info(env, info, NULL, NULL, NULL, &data) != napi_ok) return NULL;
    WendyCoreLibrary const *library = (WendyCoreLibrary const *)data;
    napi_value result;
    if (napi_create_string_utf8(env, library->version(), NAPI_AUTO_LENGTH, &result) != napi_ok) return NULL;
    return result;
}

/* Each function owns its loader wrapper; extracted functions remain valid
   independently of the object returned by open(). */
static napi_value operation(napi_env env, char const *name, napi_callback callback, WendyCoreLibrary const *source) {
    WendyCoreLibrary *library = malloc(sizeof(*library));
    if (library == NULL) { napi_throw_error(env, NULL, "Out of memory"); return NULL; }
    *library = *source;
    napi_value function;
    if (napi_create_function(env, name, NAPI_AUTO_LENGTH, callback, library, &function) != napi_ok ||
        napi_add_finalizer(env, function, library, dispose, NULL, NULL) != napi_ok) {
        free(library); return NULL;
    }
    return function;
}

static napi_value openLibrary(napi_env env, napi_callback_info info) {
    size_t count = 1, length;
    napi_value argument;
    if (napi_get_cb_info(env, info, &count, &argument, NULL, NULL) != napi_ok || count != 1 ||
        napi_get_value_string_utf8(env, argument, NULL, 0, &length) != napi_ok) {
        napi_throw_type_error(env, NULL, "Expected a native library path");
        return NULL;
    }
    char *path = (char *)malloc(length + 1);
    if (path == NULL) { napi_throw_error(env, NULL, "Out of memory"); return NULL; }
    if (napi_get_value_string_utf8(env, argument, path, length + 1, &length) != napi_ok) {
        free(path); return NULL;
    }
    WendyCoreLibrary *library = WendyCoreLibraryOpen(path);
    free(path);
    if (library == NULL) { napi_throw_error(env, NULL, "Cannot load Wendy Core native library"); return NULL; }
    napi_value result, helloFunction, versionFunction;
    if (napi_create_object(env, &result) != napi_ok) { free(library); return NULL; }
    helloFunction = operation(env, "WendyHelloWorld", hello, library);
    versionFunction = operation(env, "WendyCoreVersion", version, library);
    free(library);
    if (helloFunction == NULL || versionFunction == NULL ||
        napi_set_named_property(env, result, "WendyHelloWorld", helloFunction) != napi_ok ||
        napi_set_named_property(env, result, "WendyCoreVersion", versionFunction) != napi_ok) return NULL;
    return result;
}

NAPI_MODULE_INIT() {
    napi_value open;
    if (napi_create_function(env, "open", NAPI_AUTO_LENGTH, openLibrary, NULL, &open) != napi_ok ||
        napi_set_named_property(env, exports, "open", open) != napi_ok) return NULL;
    return exports;
}
