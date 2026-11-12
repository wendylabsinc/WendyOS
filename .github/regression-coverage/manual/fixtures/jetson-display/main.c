/* Offscreen EGL/GLES regression for a display-only Jetson app (#1666).
 * Uses EGL_EXT_platform_device, with no compositor or window interaction.
 * https://registry.khronos.org/EGL/extensions/EXT/EGL_EXT_platform_device.txt
 */
#include <EGL/egl.h>
#include <EGL/eglext.h>
#include <GLES2/gl2.h>
#include <stdio.h>
#include <string.h>

static int fail(const char *stage) {
    fprintf(stderr, "FAIL: %s; EGL=0x%x GL=0x%x\n", stage, eglGetError(), glGetError());
    return 1;
}

int main(void) {
    setvbuf(stdout, NULL, _IONBF, 0);
    puts("Starting display-only NVIDIA EGL probe");
    PFNEGLQUERYDEVICESEXTPROC query = (PFNEGLQUERYDEVICESEXTPROC)eglGetProcAddress("eglQueryDevicesEXT");
    PFNEGLGETPLATFORMDISPLAYEXTPROC platform = (PFNEGLGETPLATFORMDISPLAYEXTPROC)eglGetProcAddress("eglGetPlatformDisplayEXT");
    if (!query || !platform) return fail("EGL device extensions unavailable");
    EGLDeviceEXT devices[16];
    EGLint count = 0;
    if (!query(16, devices, &count) || count == 0) return fail("no EGL devices");
    EGLDisplay display = EGL_NO_DISPLAY;
    for (EGLint i = 0; i < count; i++) {
        EGLDisplay candidate = platform(EGL_PLATFORM_DEVICE_EXT, devices[i], NULL);
        if (candidate == EGL_NO_DISPLAY || !eglInitialize(candidate, NULL, NULL)) continue;
        const char *vendor = eglQueryString(candidate, EGL_VENDOR);
        printf("EGL candidate vendor: %s\n", vendor ? vendor : "missing");
        if (vendor && strstr(vendor, "NVIDIA")) { display = candidate; break; }
        eglTerminate(candidate);
    }
    if (display == EGL_NO_DISPLAY) return fail("no NVIDIA EGL display; software fallback is not accepted");
    if (!eglBindAPI(EGL_OPENGL_ES_API)) return fail("bind GLES");
    const EGLint attributes[] = {EGL_SURFACE_TYPE, EGL_PBUFFER_BIT, EGL_RENDERABLE_TYPE, EGL_OPENGL_ES2_BIT,
        EGL_RED_SIZE, 8, EGL_GREEN_SIZE, 8, EGL_BLUE_SIZE, 8, EGL_ALPHA_SIZE, 8, EGL_NONE};
    EGLConfig config;
    EGLint found = 0;
    if (!eglChooseConfig(display, attributes, &config, 1, &found) || found != 1) return fail("choose RGBA8 pbuffer");
    const EGLint surface_attributes[] = {EGL_WIDTH, 16, EGL_HEIGHT, 16, EGL_NONE};
    EGLSurface surface = eglCreatePbufferSurface(display, config, surface_attributes);
    const EGLint context_attributes[] = {EGL_CONTEXT_CLIENT_VERSION, 2, EGL_NONE};
    EGLContext context = eglCreateContext(display, config, EGL_NO_CONTEXT, context_attributes);
    if (surface == EGL_NO_SURFACE || context == EGL_NO_CONTEXT || !eglMakeCurrent(display, surface, surface, context)) return fail("create/current pbuffer context");
    const char *vendor = (const char *)glGetString(GL_VENDOR);
    const char *renderer = (const char *)glGetString(GL_RENDERER);
    printf("GL vendor: %s; renderer: %s\n", vendor ? vendor : "missing", renderer ? renderer : "missing");
    if (!vendor || !strstr(vendor, "NVIDIA") || !renderer || !*renderer) return fail("not an NVIDIA renderer");
    glDisable(GL_DITHER);
    glViewport(0, 0, 16, 16);
    /* Two distinct clears reject stale/constant readback. Test every pixel. */
    for (int pass = 0; pass < 2; pass++) {
        unsigned char pixels[16 * 16 * 4];
        memset(pixels, 0x7f, sizeof(pixels));
        glClearColor(pass == 0 ? 1.0f : 0.0f, pass == 0 ? 0.0f : 1.0f, 0.0f, 1.0f);
        glClear(GL_COLOR_BUFFER_BIT);
        glFinish();
        glReadPixels(0, 0, 16, 16, GL_RGBA, GL_UNSIGNED_BYTE, pixels);
        if (glGetError() != GL_NO_ERROR) return fail("render/readback");
        for (int i = 0; i < 16 * 16; i++) {
            if (pixels[i*4] != (pass == 0 ? 255 : 0) || pixels[i*4+1] != (pass == 0 ? 0 : 255) || pixels[i*4+2] != 0 || pixels[i*4+3] != 255) return fail("wrong framebuffer pixel");
        }
    }
    if (!eglMakeCurrent(display, EGL_NO_SURFACE, EGL_NO_SURFACE, EGL_NO_CONTEXT) ||
        !eglDestroyContext(display, context) || !eglDestroySurface(display, surface) || !eglTerminate(display)) return fail("EGL cleanup");
    puts("PASS: display-only NVIDIA EGL rendered and verified 512 pixels");
    return 0;
}
