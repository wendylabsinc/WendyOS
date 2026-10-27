#ifndef WENDY_CORE_NATIVE_LOADER_H
#define WENDY_CORE_NATIVE_LOADER_H

/* Private packaging glue, not a second public Core interface. */
#include "WendyCore.h"
#include <stdlib.h>
#include <string.h>

#if defined(_WIN32)
#include <windows.h>
#else
#include <dlfcn.h>
#endif

typedef struct WendyCoreLibrary {
    int32_t (WENDY_CORE_CALL *hello)(void);
    char *(WENDY_CORE_CALL *version)(void);
} WendyCoreLibrary;

static WendyCoreLibrary *WendyCoreLibraryOpen(char const *path) {
    WendyCoreLibrary *library = (WendyCoreLibrary *)malloc(sizeof(WendyCoreLibrary));
    if (library == NULL) return NULL;
#if defined(_WIN32)
    int const length = MultiByteToWideChar(CP_UTF8, MB_ERR_INVALID_CHARS, path, -1, NULL, 0);
    wchar_t *widePath = length > 0 ? (wchar_t *)malloc((size_t)length * sizeof(wchar_t)) : NULL;
    if (widePath == NULL) { free(library); return NULL; }
    MultiByteToWideChar(CP_UTF8, MB_ERR_INVALID_CHARS, path, -1, widePath, length);
    HMODULE const handle = LoadLibraryW(widePath);
    free(widePath);
    FARPROC const symbol = handle == NULL ? NULL : GetProcAddress(handle, "WendyHelloWorld");
#else
    void *handle = dlopen(path, RTLD_NOW | RTLD_LOCAL);
    void *symbol = handle == NULL ? NULL : dlsym(handle, "WendyHelloWorld");
#endif
    if (symbol == NULL) { free(library); return NULL; }
    memcpy(&library->hello, &symbol, sizeof(library->hello));
    #if defined(_WIN32)
    FARPROC const version = GetProcAddress(handle, "WendyCoreVersion");
#else
    void *version = dlsym(handle, "WendyCoreVersion");
#endif
    if (version == NULL) { free(library); return NULL; }
    memcpy(&library->version, &version, sizeof(library->version));
    /* Go shared libraries cannot safely be unloaded. Keep the OS library
       reference for the process lifetime, independently of wrapper lifetimes. */
    return library;
}

#endif
