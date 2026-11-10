package dev.wendy.android

import android.annotation.SuppressLint
import android.content.Context
import android.graphics.Bitmap
import android.graphics.Canvas
import android.graphics.Paint
import android.graphics.Path
import android.opengl.GLES20
import android.opengl.GLSurfaceView
import android.opengl.Matrix
import android.view.GestureDetector
import android.view.MotionEvent
import android.view.ScaleGestureDetector
import androidx.compose.runtime.*
import androidx.compose.ui.Modifier
import androidx.compose.ui.viewinterop.AndroidView
import java.nio.ByteBuffer
import java.nio.ByteOrder
import java.nio.FloatBuffer
import javax.microedition.khronos.egl.EGLConfig
import javax.microedition.khronos.opengles.GL10
import kotlin.math.cos
import kotlin.math.sin
import kotlin.math.sqrt

/** A camera marker in the scene; [frustum] is its apex then four far corners when the camera has an orientation. */
class SiteMarker(val id: String, val position: FloatArray, val color: FloatArray, val frustum: FloatArray? = null)

/**
 * Builds the marker for a camera: at its aligned pose with a view cone through its lens corners when it has
 * [geometry] (a gateway alignment or manual visualization), otherwise at its manual placement.
 */
fun siteMarker(camera: SiteCamera, selected: Boolean, placing: Boolean, geometry: SiteCameraGeometry? = null): SiteMarker? {
    val color = if (placing) floatArrayOf(1f, .58f, 0f) else if (selected) floatArrayOf(.62f, .89f, .75f) else floatArrayOf(.16f, .44f, .31f)
    if (geometry != null && !placing) {
        val p = geometry.pose.position; val r = geometry.pose.matrix(); val lens = geometry.lens
        val depth = 1.2
        val corners = lens?.let { l -> listOf(l.ray(0.0, 0.0), l.ray(l.width.toDouble(), 0.0), l.ray(l.width.toDouble(), l.height.toDouble()), l.ray(0.0, l.height.toDouble())) }
        val frustum = corners?.takeIf { c -> c.all { it != null && it[2] > 0.05 } }?.let { c ->
            val apex = doubleArrayOf(p.x, p.y, p.z)
            (listOf(apex) + c.map { ray -> SiteMath.apply3(r, doubleArrayOf(ray!![0] / ray[2] * depth, ray[1] / ray[2] * depth, depth)).let { w -> doubleArrayOf(apex[0] + w[0], apex[1] + w[1], apex[2] + w[2]) } })
                .flatMap { it.map(Double::toFloat) }.toFloatArray()
        }
        return SiteMarker(camera.id, floatArrayOf(p.x.toFloat(), p.y.toFloat(), p.z.toFloat()), color, frustum)
    }
    val p = camera.position ?: return null
    val frustum = camera.manualAlignment?.let { manual ->
        val r = manual.rotation()
        val depth = 1.2
        val h = tan(Math.toRadians(manual.horizontalFOV) / 2) * depth
        val v = tan(Math.toRadians(manual.verticalFOV) / 2) * depth
        val apex = doubleArrayOf(p.x, p.y, p.z)
        val corners = listOf(doubleArrayOf(-h, -v, depth), doubleArrayOf(h, -v, depth), doubleArrayOf(h, v, depth), doubleArrayOf(-h, v, depth)).map { local ->
            SiteMath.apply3(r, local).let { w -> doubleArrayOf(apex[0] + w[0], apex[1] + w[1], apex[2] + w[2]) }
        }
        (listOf(apex) + corners).flatMap { it.map(Double::toFloat) }.toFloatArray()
    }
    return SiteMarker(camera.id, floatArrayOf(p.x.toFloat(), p.y.toFloat(), p.z.toFloat()), color, frustum)
}
private fun tan(radians: Double) = kotlin.math.tan(radians)

/**
 * The site model in 3D (iOS SiteScene, drawn with OpenGL ES). Drag to orbit, pinch to zoom, drag with two
 * fingers to pan, double-tap to reset. A tap selects a camera marker, or places one on the model while placing.
 */
@Composable
fun SiteSceneView(geometry: SiteGeometry, markers: List<SiteMarker>, placing: Boolean, onSelect: (String) -> Unit, onPlace: (SitePoint) -> Unit, modifier: Modifier = Modifier) {
    val select by rememberUpdatedState(onSelect)
    val place by rememberUpdatedState(onPlace)
    val orbit = remember(geometry) { SiteOrbit() }
    // One GL surface at a time: a covered page releases its view and rebuilds it, at the same orbit, on return.
    if (!LocalPageVisible.current) { androidx.compose.foundation.layout.Box(modifier); return }
    AndroidView(factory = { context -> SiteGLView(context, orbit).also { view -> view.callbacks = Pair({ id: String -> select(id) }, { point: SitePoint -> place(point) }) } }, modifier = modifier,
        onReset = { it.resume() }, update = { view -> view.resume(); view.show(geometry, markers, placing) }, onRelease = { it.pause() })
}

/** Where the person is looking from; it outlives the GL view so returning to a page keeps the view. */
class SiteOrbit { var yaw = 35f; var pitch = 30f; var distance = 6f; val target = FloatArray(3); var framed = false }

@SuppressLint("ViewConstructor", "ClickableViewAccessibility")
private class SiteGLView(context: Context, private val orbit: SiteOrbit) : GLTextureView(context) {
    var callbacks: Pair<(String) -> Unit, (SitePoint) -> Unit>? = null
    private var paused = false
    fun pause() { paused = true }
    fun resume() { if (paused) { paused = false; requestRender() } }
    private val renderer = SiteRendererGL()
    private var geometry: SiteGeometry? = null
    private var markers: List<SiteMarker> = emptyList()
    private var placing = false
    private var yaw by orbit::yaw
    private var pitch by orbit::pitch
    private var distance by orbit::distance
    private val target get() = orbit.target
    private var lastX = 0f; private var lastY = 0f; private var pointers = 0

    init {
        setRenderer(renderer)
        contentDescription = "3D site model"
    }

    fun show(next: SiteGeometry, nextMarkers: List<SiteMarker>, nextPlacing: Boolean) {
        if (next !== geometry) { geometry = next; if (!orbit.framed) reset(); queueEvent { renderer.load(next) } }
        markers = nextMarkers; placing = nextPlacing
        queueEvent { renderer.markers = nextMarkers }
        update()
    }
    /** Frames the whole model in the narrower of the two fields of view, so portrait screens fit it too. */
    private fun reset() {
        val g = geometry ?: return
        val aspect = if (width > 0 && height > 0) width.toFloat() / height else 1f
        val vertical = Math.toRadians(25.0)
        val horizontal = kotlin.math.atan(kotlin.math.tan(vertical) * aspect)
        g.center.copyInto(target); distance = (g.radius / kotlin.math.sin(minOf(vertical, horizontal)) * 1.05).toFloat(); yaw = 35f; pitch = 30f
        orbit.framed = width > 0 && height > 0
    }
    override fun onSizeChanged(w: Int, h: Int, oldw: Int, oldh: Int) { super.onSizeChanged(w, h, oldw, oldh); if ((oldw == 0 || oldh == 0) && !orbit.framed) reset(); update() }
    private fun eye(): FloatArray {
        val p = Math.toRadians(pitch.toDouble()); val y = Math.toRadians(yaw.toDouble())
        return floatArrayOf(target[0] + (distance * cos(p) * sin(y)).toFloat(), target[1] + (distance * sin(p)).toFloat(), target[2] + (distance * cos(p) * cos(y)).toFloat())
    }
    private fun update() {
        val e = eye(); val t = target.copyOf(); val d = distance
        queueEvent { renderer.camera(e, t, d) }
        requestRender()
    }

    private val scale = ScaleGestureDetector(context, object : ScaleGestureDetector.SimpleOnScaleGestureListener() {
        override fun onScale(detector: ScaleGestureDetector): Boolean {
            val g = geometry ?: return true
            distance = (distance / detector.scaleFactor).coerceIn(g.radius * 0.05f, g.radius * 12f); update(); return true
        }
    })
    private val gestures = GestureDetector(context, object : GestureDetector.SimpleOnGestureListener() {
        override fun onSingleTapUp(e: MotionEvent): Boolean { tap(e.x, e.y); return true }
        override fun onDoubleTap(e: MotionEvent): Boolean { orbit.framed = false; reset(); update(); return true }
    })

    override fun onTouchEvent(event: MotionEvent): Boolean {
        scale.onTouchEvent(event); gestures.onTouchEvent(event)
        parent?.requestDisallowInterceptTouchEvent(true)
        val count = event.pointerCount
        val x = (0 until count).map { event.getX(it) }.average().toFloat(); val y = (0 until count).map { event.getY(it) }.average().toFloat()
        when (event.actionMasked) {
            MotionEvent.ACTION_DOWN, MotionEvent.ACTION_POINTER_DOWN, MotionEvent.ACTION_POINTER_UP -> { lastX = x; lastY = y; pointers = count }
            MotionEvent.ACTION_MOVE -> {
                if (count != pointers) { lastX = x; lastY = y; pointers = count }
                val dx = x - lastX; val dy = y - lastY
                if (count == 1) { yaw -= dx * 0.35f; pitch = (pitch + dy * 0.35f).coerceIn(-85f, 85f) }
                else if (!scale.isInProgress || count == 2) pan(dx, dy)
                lastX = x; lastY = y; update()
            }
        }
        return true
    }
    private fun pan(dx: Float, dy: Float) {
        val y = Math.toRadians(yaw.toDouble())
        val unit = distance / height.coerceAtLeast(1) * 1.2f
        val right = floatArrayOf(cos(y).toFloat(), 0f, (-sin(y)).toFloat())
        target[0] -= right[0] * dx * unit; target[2] -= right[2] * dx * unit; target[1] += dy * unit
    }

    private fun viewProjection(): FloatArray {
        val e = eye(); val view = FloatArray(16); val projection = FloatArray(16); val vp = FloatArray(16)
        Matrix.setLookAtM(view, 0, e[0], e[1], e[2], target[0], target[1], target[2], 0f, 1f, 0f)
        Matrix.perspectiveM(projection, 0, 50f, width.toFloat() / height.coerceAtLeast(1), maxOf(0.01f, distance * 0.01f), distance * 40f + 100f)
        Matrix.multiplyMM(vp, 0, projection, 0, view, 0)
        return vp
    }
    private fun tap(x: Float, y: Float) {
        val (select, place) = callbacks ?: return
        val vp = viewProjection()
        if (!placing) {
            // The nearest marker within 32 dp of the tap.
            val limit = 32 * resources.displayMetrics.density
            markers.mapNotNull { m ->
                val clip = FloatArray(4); Matrix.multiplyMV(clip, 0, vp, 0, floatArrayOf(m.position[0], m.position[1], m.position[2], 1f), 0)
                if (clip[3] <= 0) return@mapNotNull null
                val sx = (clip[0] / clip[3] + 1) / 2 * width; val sy = (1 - clip[1] / clip[3]) / 2 * height
                val d = sqrt((sx - x) * (sx - x) + (sy - y) * (sy - y))
                if (d <= limit) m.id to d else null
            }.minByOrNull { it.second }?.let { select(it.first) }
            return
        }
        val inverse = FloatArray(16); if (!Matrix.invertM(inverse, 0, vp, 0)) return
        val nx = x / width * 2 - 1; val ny = 1 - y / height * 2
        fun unproject(z: Float): FloatArray { val out = FloatArray(4); Matrix.multiplyMV(out, 0, inverse, 0, floatArrayOf(nx, ny, z, 1f), 0); return floatArrayOf(out[0] / out[3], out[1] / out[3], out[2] / out[3]) }
        val near = unproject(-1f); val far = unproject(1f)
        val direction = floatArrayOf(far[0] - near[0], far[1] - near[1], far[2] - near[2])
        val g = geometry ?: return
        SiteRay.nearest(g, near, direction)?.let { place(SitePoint(it[0].toDouble(), it[1].toDouble(), it[2].toDouble())) }
    }
}

/** Ray and triangle intersection (Möller–Trumbore), for placing markers on the model. */
object SiteRay {
    fun nearest(geometry: SiteGeometry, origin: FloatArray, direction: FloatArray): FloatArray? {
        var best = Float.MAX_VALUE
        for (mesh in geometry.meshes) {
            val p = mesh.positions
            var i = 0
            while (i + 8 < p.size) {
                val t = intersect(origin, direction, p, i)
                if (t != null && t > 0 && t < best) best = t
                i += 9
            }
        }
        if (best == Float.MAX_VALUE) return null
        return floatArrayOf(origin[0] + direction[0] * best, origin[1] + direction[1] * best, origin[2] + direction[2] * best)
    }
    private fun intersect(o: FloatArray, d: FloatArray, p: FloatArray, i: Int): Float? {
        val e1x = p[i + 3] - p[i]; val e1y = p[i + 4] - p[i + 1]; val e1z = p[i + 5] - p[i + 2]
        val e2x = p[i + 6] - p[i]; val e2y = p[i + 7] - p[i + 1]; val e2z = p[i + 8] - p[i + 2]
        val hx = d[1] * e2z - d[2] * e2y; val hy = d[2] * e2x - d[0] * e2z; val hz = d[0] * e2y - d[1] * e2x
        val a = e1x * hx + e1y * hy + e1z * hz
        if (a > -1e-9f && a < 1e-9f) return null
        val f = 1 / a
        val sx = o[0] - p[i]; val sy = o[1] - p[i + 1]; val sz = o[2] - p[i + 2]
        val u = f * (sx * hx + sy * hy + sz * hz)
        if (u < 0 || u > 1) return null
        val qx = sy * e1z - sz * e1y; val qy = sz * e1x - sx * e1z; val qz = sx * e1y - sy * e1x
        val v = f * (d[0] * qx + d[1] * qy + d[2] * qz)
        if (v < 0 || u + v > 1) return null
        return f * (e2x * qx + e2y * qy + e2z * qz)
    }
}

private class SiteRendererGL : GLSurfaceView.Renderer {
    private class Uploaded(val buffer: Int, val count: Int, val color: FloatArray)
    private var meshes = emptyList<Uploaded>()
    private var pending: SiteGeometry? = null
    var markers: List<SiteMarker> = emptyList()
    private var program = 0
    private val view = FloatArray(16); private val projection = FloatArray(16); private val vp = FloatArray(16)
    private var eye = floatArrayOf(0f, 2f, 6f); private var target = FloatArray(3); private var distance = 6f
    private var aspect = 1f
    private val cube by lazy { floatBuffer(cubeVertices()) }

    fun load(geometry: SiteGeometry) { pending = geometry }
    fun camera(e: FloatArray, t: FloatArray, d: Float) { eye = e; target = t; distance = d }

    override fun onSurfaceCreated(gl: GL10?, config: EGLConfig?) {
        GLES20.glClearColor(0.94f, 0.93f, 0.90f, 1f)
        GLES20.glEnable(GLES20.GL_DEPTH_TEST)
        program = link("""
            uniform mat4 uMVP; attribute vec3 aPosition; attribute vec3 aNormal; varying float vLight;
            void main() { gl_Position = uMVP * vec4(aPosition, 1.0); vLight = 0.38 + 0.62 * abs(dot(normalize(aNormal + vec3(0.0, 1e-6, 0.0)), normalize(vec3(0.35, 0.9, 0.25)))); }
        """, """
            precision mediump float; uniform vec3 uColor; uniform float uLit; varying float vLight;
            void main() { gl_FragColor = vec4(uColor * mix(1.0, vLight, uLit), 1.0); }
        """)
        meshes = emptyList()
        pending = pending ?: lastGeometry
    }
    private var lastGeometry: SiteGeometry? = null
    override fun onSurfaceChanged(gl: GL10?, width: Int, height: Int) { GLES20.glViewport(0, 0, width, height); aspect = width.toFloat() / height.coerceAtLeast(1) }

    override fun onDrawFrame(gl: GL10?) {
        pending?.let { upload(it); lastGeometry = it; pending = null }
        GLES20.glClear(GLES20.GL_COLOR_BUFFER_BIT or GLES20.GL_DEPTH_BUFFER_BIT)
        Matrix.setLookAtM(view, 0, eye[0], eye[1], eye[2], target[0], target[1], target[2], 0f, 1f, 0f)
        Matrix.perspectiveM(projection, 0, 50f, aspect, maxOf(0.01f, distance * 0.01f), distance * 40f + 100f)
        Matrix.multiplyMM(vp, 0, projection, 0, view, 0)
        GLES20.glUseProgram(program)
        val position = GLES20.glGetAttribLocation(program, "aPosition"); val normal = GLES20.glGetAttribLocation(program, "aNormal")
        val mvp = GLES20.glGetUniformLocation(program, "uMVP"); val color = GLES20.glGetUniformLocation(program, "uColor"); val lit = GLES20.glGetUniformLocation(program, "uLit")
        GLES20.glUniformMatrix4fv(mvp, 1, false, vp, 0)
        GLES20.glUniform1f(lit, 1f)
        GLES20.glEnableVertexAttribArray(position); GLES20.glEnableVertexAttribArray(normal)
        for (mesh in meshes) {
            GLES20.glBindBuffer(GLES20.GL_ARRAY_BUFFER, mesh.buffer)
            GLES20.glVertexAttribPointer(position, 3, GLES20.GL_FLOAT, false, 24, 0)
            GLES20.glVertexAttribPointer(normal, 3, GLES20.GL_FLOAT, false, 24, 12)
            GLES20.glUniform3fv(color, 1, mesh.color, 0)
            GLES20.glDrawArrays(GLES20.GL_TRIANGLES, 0, mesh.count)
        }
        GLES20.glBindBuffer(GLES20.GL_ARRAY_BUFFER, 0)
        // Markers stay readable at any zoom: a fixed fraction of the view distance.
        val size = maxOf(0.06f, distance * 0.025f)
        for (marker in markers) {
            val model = FloatArray(16); Matrix.setIdentityM(model, 0)
            Matrix.translateM(model, 0, marker.position[0], marker.position[1], marker.position[2]); Matrix.scaleM(model, 0, size, size, size)
            val combined = FloatArray(16); Matrix.multiplyMM(combined, 0, vp, 0, model, 0)
            GLES20.glUniformMatrix4fv(mvp, 1, false, combined, 0)
            GLES20.glUniform3fv(color, 1, marker.color, 0)
            cube.position(0); GLES20.glVertexAttribPointer(position, 3, GLES20.GL_FLOAT, false, 24, cube)
            cube.position(3); GLES20.glVertexAttribPointer(normal, 3, GLES20.GL_FLOAT, false, 24, cube)
            GLES20.glDrawArrays(GLES20.GL_TRIANGLES, 0, 36)
            marker.frustum?.let { f ->
                GLES20.glUniformMatrix4fv(mvp, 1, false, vp, 0)
                GLES20.glUniform1f(lit, 0f)
                val lines = FloatArray(16 * 6)
                val pairs = listOf(0 to 1, 0 to 2, 0 to 3, 0 to 4, 1 to 2, 2 to 3, 3 to 4, 4 to 1)
                pairs.forEachIndexed { k, (a, b) -> for (axis in 0..2) { lines[k * 12 + axis] = f[a * 3 + axis]; lines[k * 12 + 6 + axis] = f[b * 3 + axis] } }
                val buffer = floatBuffer(lines)
                buffer.position(0); GLES20.glVertexAttribPointer(position, 3, GLES20.GL_FLOAT, false, 24, buffer)
                buffer.position(3); GLES20.glVertexAttribPointer(normal, 3, GLES20.GL_FLOAT, false, 24, buffer)
                GLES20.glLineWidth(3f)
                GLES20.glDrawArrays(GLES20.GL_LINES, 0, 16)
                GLES20.glUniform1f(lit, 1f)
            }
        }
        GLES20.glDisableVertexAttribArray(position); GLES20.glDisableVertexAttribArray(normal)
    }

    private fun upload(geometry: SiteGeometry) {
        if (meshes.isNotEmpty()) GLES20.glDeleteBuffers(meshes.size, meshes.map { it.buffer }.toIntArray(), 0)
        val ids = IntArray(geometry.meshes.size)
        if (ids.isNotEmpty()) GLES20.glGenBuffers(ids.size, ids, 0)
        meshes = geometry.meshes.mapIndexed { index, mesh ->
            val interleaved = FloatArray(mesh.positions.size * 2)
            for (v in 0 until mesh.positions.size / 3) {
                for (axis in 0..2) { interleaved[v * 6 + axis] = mesh.positions[v * 3 + axis]; interleaved[v * 6 + 3 + axis] = mesh.normals[v * 3 + axis] }
            }
            GLES20.glBindBuffer(GLES20.GL_ARRAY_BUFFER, ids[index])
            GLES20.glBufferData(GLES20.GL_ARRAY_BUFFER, interleaved.size * 4, floatBuffer(interleaved), GLES20.GL_STATIC_DRAW)
            Uploaded(ids[index], mesh.positions.size / 3, mesh.color)
        }
        GLES20.glBindBuffer(GLES20.GL_ARRAY_BUFFER, 0)
    }

    private fun link(vertex: String, fragment: String): Int {
        fun compile(type: Int, source: String) = GLES20.glCreateShader(type).also { GLES20.glShaderSource(it, source); GLES20.glCompileShader(it) }
        return GLES20.glCreateProgram().also { p ->
            GLES20.glAttachShader(p, compile(GLES20.GL_VERTEX_SHADER, vertex)); GLES20.glAttachShader(p, compile(GLES20.GL_FRAGMENT_SHADER, fragment)); GLES20.glLinkProgram(p)
        }
    }
    private fun floatBuffer(values: FloatArray): FloatBuffer = ByteBuffer.allocateDirect(values.size * 4).order(ByteOrder.nativeOrder()).asFloatBuffer().apply { put(values); position(0) }
    private fun cubeVertices(): FloatArray {
        val faces = listOf(floatArrayOf(0f, 0f, 1f), floatArrayOf(0f, 0f, -1f), floatArrayOf(1f, 0f, 0f), floatArrayOf(-1f, 0f, 0f), floatArrayOf(0f, 1f, 0f), floatArrayOf(0f, -1f, 0f))
        val out = ArrayList<Float>()
        for (n in faces) {
            val (u, v) = when { n[0] != 0f -> floatArrayOf(0f, 1f, 0f) to floatArrayOf(0f, 0f, 1f); n[1] != 0f -> floatArrayOf(1f, 0f, 0f) to floatArrayOf(0f, 0f, 1f); else -> floatArrayOf(1f, 0f, 0f) to floatArrayOf(0f, 1f, 0f) }
            val corners = listOf(-1f to -1f, 1f to -1f, 1f to 1f, -1f to -1f, 1f to 1f, -1f to 1f)
            for ((a, b) in corners) { for (axis in 0..2) out += n[axis] + u[axis] * a + v[axis] * b; out += n[0]; out += n[1]; out += n[2] }
        }
        return out.toFloatArray()
    }
}

/** A small preview drawn on the CPU: triangles sorted back to front and shaded by their normal (iOS model thumbnails). */
object SiteThumbnail {
    fun render(geometry: SiteGeometry, width: Int, height: Int, background: Int = 0x00000000): Bitmap {
        val bitmap = Bitmap.createBitmap(width, height, Bitmap.Config.ARGB_8888)
        val canvas = Canvas(bitmap); canvas.drawColor(background)
        val center = geometry.center; val radius = geometry.radius
        val yaw = Math.toRadians(35.0); val pitch = Math.toRadians(35.0)
        val forward = doubleArrayOf(-cos(pitch) * sin(yaw), -sin(pitch), -cos(pitch) * cos(yaw))
        val right = doubleArrayOf(cos(yaw), 0.0, -sin(yaw))
        val up = doubleArrayOf(right[1] * forward[2] - right[2] * forward[1], right[2] * forward[0] - right[0] * forward[2], right[0] * forward[1] - right[1] * forward[0]).let { u -> u.map { -it }.toDoubleArray() }
        val scale = minOf(width, height) / (2.2 * radius)
        class Triangle(val xs: FloatArray, val ys: FloatArray, val depth: Double, val color: Int)
        val triangles = ArrayList<Triangle>(geometry.triangles)
        for (mesh in geometry.meshes) {
            val p = mesh.positions; var i = 0
            while (i + 8 < p.size) {
                val xs = FloatArray(3); val ys = FloatArray(3); var depth = 0.0
                for (v in 0..2) {
                    val x = p[i + v * 3] - center[0]; val y = p[i + v * 3 + 1] - center[1]; val z = p[i + v * 3 + 2] - center[2]
                    xs[v] = (width / 2 + (x * right[0] + y * right[1] + z * right[2]) * scale).toFloat()
                    ys[v] = (height / 2 - (x * up[0] + y * up[1] + z * up[2]) * scale).toFloat()
                    depth += x * forward[0] + y * forward[1] + z * forward[2]
                }
                val n = mesh.normals
                val light = 0.38 + 0.62 * kotlin.math.abs(n[i] * 0.35 + n[i + 1] * 0.9 + n[i + 2] * 0.25) / 1.0
                val c = mesh.color.map { (it * light * 255).toInt().coerceIn(0, 255) }
                triangles += Triangle(xs, ys, depth, (0xFF shl 24) or (c[0] shl 16) or (c[1] shl 8) or c[2])
                i += 9
            }
        }
        triangles.sortByDescending { it.depth }
        val paint = Paint(Paint.ANTI_ALIAS_FLAG).apply { style = Paint.Style.FILL_AND_STROKE; strokeWidth = 0.6f }
        val path = Path()
        for (t in triangles) {
            path.reset(); path.moveTo(t.xs[0], t.ys[0]); path.lineTo(t.xs[1], t.ys[1]); path.lineTo(t.xs[2], t.ys[2]); path.close()
            paint.color = t.color; canvas.drawPath(path, paint)
        }
        return bitmap
    }
}
