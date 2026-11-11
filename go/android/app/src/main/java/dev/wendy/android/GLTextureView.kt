package dev.wendy.android

import android.content.Context
import android.graphics.SurfaceTexture
import android.opengl.EGL14
import android.opengl.EGLConfig
import android.opengl.EGLContext
import android.opengl.EGLDisplay
import android.opengl.EGLSurface
import android.opengl.GLSurfaceView
import android.view.TextureView
import java.util.concurrent.LinkedBlockingQueue

/**
 * OpenGL ES 2 on a TextureView with its own render thread. Unlike GLSurfaceView it draws inside the normal
 * view tree, so it composes with Compose pages stacked above or below it.
 */
open class GLTextureView(context: Context) : TextureView(context), TextureView.SurfaceTextureListener {
    private var renderer: GLSurfaceView.Renderer? = null
    private var thread: RenderThread? = null
    private val pending = LinkedBlockingQueue<Runnable>()

    init { surfaceTextureListener = this; isOpaque = true }

    fun setRenderer(value: GLSurfaceView.Renderer) { renderer = value }
    /** Runs [event] on the render thread before the next frame. */
    fun queueEvent(event: Runnable) { pending.add(event); thread?.wake() }
    fun requestRender() { thread?.wake() }
    open fun onPause() {}
    open fun onResume() {}

    override fun onSurfaceTextureAvailable(surface: SurfaceTexture, width: Int, height: Int) {
        val r = renderer ?: return
        thread = RenderThread(surface, r, pending, width, height).also { it.start() }
    }
    override fun onSurfaceTextureSizeChanged(surface: SurfaceTexture, width: Int, height: Int) { thread?.resize(width, height) }
    override fun onSurfaceTextureDestroyed(surface: SurfaceTexture): Boolean { thread?.finish(); thread = null; return true }
    override fun onSurfaceTextureUpdated(surface: SurfaceTexture) {}

    private class RenderThread(private val texture: SurfaceTexture, private val renderer: GLSurfaceView.Renderer, private val events: LinkedBlockingQueue<Runnable>,
                               @Volatile private var width: Int, @Volatile private var height: Int) : Thread("SiteRenderer") {
        private val lock = Object()
        @Volatile private var running = true
        @Volatile private var dirty = true
        @Volatile private var resized = true
        fun wake() = synchronized(lock) { dirty = true; lock.notifyAll() }
        fun resize(w: Int, h: Int) = synchronized(lock) { width = w; height = h; resized = true; dirty = true; lock.notifyAll() }
        fun finish() { synchronized(lock) { running = false; lock.notifyAll() }; join(2000) }

        override fun run() {
            val display: EGLDisplay = EGL14.eglGetDisplay(EGL14.EGL_DEFAULT_DISPLAY)
            val version = IntArray(2)
            if (!EGL14.eglInitialize(display, version, 0, version, 1)) return
            // OpenGL ES 3 when available (camera projection needs depth textures and derivatives); ES 2 otherwise.
            fun config(renderable: Int): EGLConfig? {
                val attributes = intArrayOf(EGL14.EGL_RED_SIZE, 8, EGL14.EGL_GREEN_SIZE, 8, EGL14.EGL_BLUE_SIZE, 8, EGL14.EGL_ALPHA_SIZE, 8, EGL14.EGL_DEPTH_SIZE, 16,
                    EGL14.EGL_RENDERABLE_TYPE, renderable, EGL14.EGL_NONE)
                val configs = arrayOfNulls<EGLConfig>(1); val count = IntArray(1)
                return if (EGL14.eglChooseConfig(display, attributes, 0, configs, 0, 1, count, 0) && count[0] > 0) configs[0] else null
            }
            val es3 = config(android.opengl.EGLExt.EGL_OPENGL_ES3_BIT_KHR)
            val configs = arrayOf(es3 ?: config(EGL14.EGL_OPENGL_ES2_BIT) ?: run { EGL14.eglTerminate(display); return })
            var context: EGLContext = EGL14.EGL_NO_CONTEXT
            if (es3 != null) context = EGL14.eglCreateContext(display, configs[0], EGL14.EGL_NO_CONTEXT, intArrayOf(EGL14.EGL_CONTEXT_CLIENT_VERSION, 3, EGL14.EGL_NONE), 0)
            if (context == EGL14.EGL_NO_CONTEXT) context = EGL14.eglCreateContext(display, configs[0], EGL14.EGL_NO_CONTEXT, intArrayOf(EGL14.EGL_CONTEXT_CLIENT_VERSION, 2, EGL14.EGL_NONE), 0)
            val surface: EGLSurface = EGL14.eglCreateWindowSurface(display, configs[0], texture, intArrayOf(EGL14.EGL_NONE), 0)
            try {
                if (surface == EGL14.EGL_NO_SURFACE || !EGL14.eglMakeCurrent(display, surface, surface, context)) return
                renderer.onSurfaceCreated(null, null)
                while (true) {
                    synchronized(lock) { while (running && !dirty && events.isEmpty()) lock.wait(); if (!running) return; dirty = false }
                    while (true) { (events.poll() ?: break).run() }
                    if (resized) { resized = false; renderer.onSurfaceChanged(null, width, height) }
                    renderer.onDrawFrame(null)
                    EGL14.eglSwapBuffers(display, surface)
                }
            } catch (_: InterruptedException) {
            } finally {
                EGL14.eglMakeCurrent(display, EGL14.EGL_NO_SURFACE, EGL14.EGL_NO_SURFACE, EGL14.EGL_NO_CONTEXT)
                EGL14.eglDestroySurface(display, surface); EGL14.eglDestroyContext(display, context); EGL14.eglTerminate(display)
            }
        }
    }
}
