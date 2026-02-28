// ALSA - Swift wrapper for ALSA audio on Linux
// Uses runtime dynamic loading (dlopen) to avoid compile-time linking

#if os(Linux)
    // Re-export public types
    @_exported import struct Foundation.Data
#endif
