package dev.wendy.android

import android.graphics.Bitmap
import android.opengl.GLES20
import android.opengl.GLES30
import android.opengl.GLUtils
import android.opengl.Matrix
import kotlin.math.abs
import kotlin.math.max
import kotlin.math.sqrt

/** A camera's latest picture to project onto the model at its aligned pose (iOS SiteCameraColorRenderer input). */
class SiteProjector(val id: String, val geometry: SiteCameraGeometry, val image: Bitmap, val received: Long)

/** Projection geometry shared by the renderer and its tests. Matrices are column-major. */
object SiteProjection {
    const val SLOTS = 4
    const val DEPTH_SIZE = 1024

    /** Model world to optical camera (right, down, forward). */
    fun worldToCamera(pose: SiteCameraPose): FloatArray {
        val r = pose.matrix(); val p = pose.position
        val out = FloatArray(16)
        for (row in 0..2) {
            for (col in 0..2) out[col * 4 + row] = r[row * 3 + col].toFloat() // transpose: rows are the optical axes
            out[12 + row] = -(r[row * 3] * p.x + r[row * 3 + 1] * p.y + r[row * 3 + 2] * p.z).toFloat()
        }
        out[15] = 1f
        return out
    }

    /** The largest |x/z| and |y/z| of the lens edge rays, with 2% margin, or null for an unusable lens. */
    fun extent(lens: SiteCameraLens): FloatArray? {
        val rays = lens.boundaryRays ?: return null
        if (rays.any { it[2] <= 0.001 }) return null
        val x = rays.maxOf { abs(it[0] / it[2]) } * 1.02; val y = rays.maxOf { abs(it[1] / it[2]) } * 1.02
        return if (x > 0 && y > 0 && x.isFinite() && y.isFinite()) floatArrayOf(x.toFloat(), y.toFloat()) else null
    }

    /** Optical coordinates to clip space for the depth pass: x/extent, y/extent, perspective depth. */
    fun depthProjection(extentX: Float, extentY: Float, near: Float, far: Float) = FloatArray(16).also {
        it[0] = 1 / extentX; it[5] = 1 / extentY
        it[10] = (far + near) / (far - near); it[11] = 1f; it[14] = -2 * far * near / (far - near)
    }

    /** Beyond the farthest model corner, so nothing the camera sees is clipped. */
    fun far(geometry: SiteGeometry, pose: SiteCameraPose): Float {
        var far = 1.0
        for (x in listOf(geometry.min[0], geometry.max[0])) for (y in listOf(geometry.min[1], geometry.max[1])) for (z in listOf(geometry.min[2], geometry.max[2])) {
            val p = pose.position
            far = max(far, sqrt((x - p.x) * (x - p.x) + (y - p.y) * (y - p.y) + (z - p.z) * (z - p.z)) + 1)
        }
        return far.toFloat()
    }

    /**
     * RoomPlan walls have thickness, and a fitted optical centre may lie inside the wall the camera hangs on. Only
     * that wall (a "WallN" mesh, vertical, facing along the view, with the camera within 9 cm of it) is left out of
     * the camera's depth view, as on iOS; furniture and other walls always occlude.
     */
    fun mountingWalls(geometry: SiteGeometry, pose: SiteCameraPose): Set<Int> {
        val m = pose.matrix(); val forward = doubleArrayOf(m[6], m[7], m[8]); val p = pose.position
        val result = HashSet<Int>()
        geometry.meshes.forEachIndexed { index, mesh ->
            val name = mesh.name
            if (!name.startsWith("Wall") || name.length == 4 || !name.drop(4).all { it.isDigit() } || mesh.positions.isEmpty()) return@forEachIndexed
            val low = DoubleArray(3) { Double.MAX_VALUE }; val high = DoubleArray(3) { -Double.MAX_VALUE }
            for (i in mesh.positions.indices) { val axis = i % 3; low[axis] = minOf(low[axis], mesh.positions[i].toDouble()); high[axis] = maxOf(high[axis], mesh.positions[i].toDouble()) }
            val size = DoubleArray(3) { high[it] - low[it] }
            // A vertical wall is thin along x or z; its normal is that axis.
            val thin = if (size[0] <= size[2]) 0 else 2
            if (size[1] < 3 * size[thin]) return@forEachIndexed
            if (abs(forward[thin]) <= 0.2 || abs(forward[1]) >= 0.98) return@forEachIndexed
            val point = doubleArrayOf(p.x, p.y, p.z)
            val inside = (0..2).all { axis -> val margin = if (axis == thin) 0.09 else 0.0; point[axis] >= low[axis] - margin && point[axis] <= high[axis] + margin }
            if (inside) result += index
        }
        return result
    }

    /** The status line under the view for one projected camera (iOS SiteCameraColorRenderer.status). */
    fun status(geometry: SiteCameraGeometry, mountingWall: Boolean): String {
        val lens = geometry.lens
        val base = if (geometry.pose.provenance == "Manual visualization") "Live camera color · Manually aligned" else if (lens?.measured == true) "Live camera color" else "Live camera color · Estimated lens"
        return if (mountingWall) "$base · Mounting wall overlap" else base
    }

    const val VERTEX = """#version 300 es
uniform mat4 uMVP; in vec3 aPosition; in vec3 aNormal; out vec3 vWorld; out float vLight;
void main() {
    gl_Position = uMVP * vec4(aPosition, 1.0); vWorld = aPosition;
    vLight = 0.38 + 0.62 * abs(dot(normalize(aNormal + vec3(0.0, 1e-6, 0.0)), normalize(vec3(0.35, 0.9, 0.25))));
}"""

    /** The iOS SiteCameraColorRenderer fragment body, for four camera slots. */
    const val FRAGMENT = """#version 300 es
precision highp float;
uniform vec3 uColor; uniform int uCount;
uniform mat4 uWorldToCamera[4];
uniform vec4 uLens[4];        // fx, fy, cx, cy
uniform vec4 uSize[4];        // width, height, extent x, extent y
uniform vec4 uDistortion[4];  // k1, k2, p1, p2
uniform vec4 uExtra[4];       // k3, near, far
uniform sampler2D uCamera0; uniform sampler2D uCamera1; uniform sampler2D uCamera2; uniform sampler2D uCamera3;
uniform sampler2D uDepth0; uniform sampler2D uDepth1; uniform sampler2D uDepth2; uniform sampler2D uDepth3;
in vec3 vWorld; in float vLight; out vec4 fragColor;
float bestQuality = 0.0; vec3 projected = vec3(0.0);
void project(int i, sampler2D camera, sampler2D depthMap) {
    vec3 optical = (uWorldToCamera[i] * vec4(vWorld, 1.0)).xyz;
    vec3 planeNormal = cross(dFdx(optical), dFdy(optical));
    if (optical.z <= 0.01) return;
    vec2 p = optical.xy / optical.z;
    float r2 = dot(p, p);
    vec4 d = uDistortion[i];
    float radial = 1.0 + r2 * (d.x + r2 * (d.y + r2 * uExtra[i].x));
    vec2 distorted = p * radial + vec2(2.0 * d.z * p.x * p.y + d.w * (r2 + 2.0 * p.x * p.x), d.z * (r2 + 2.0 * p.y * p.y) + 2.0 * d.w * p.x * p.y);
    vec2 uv = (distorted * uLens[i].xy + uLens[i].zw) / uSize[i].xy;
    vec2 depthUV = 0.5 + 0.5 * p / uSize[i].zw;
    if (any(lessThan(uv, vec2(0.0))) || any(greaterThan(uv, vec2(1.0))) || any(lessThan(depthUV, vec2(0.0))) || any(greaterThan(depthUV, vec2(1.0)))) return;
    float stored = texture(depthMap, depthUV).r;
    if (stored >= 1.0) return;
    float near = uExtra[i].y; float far = uExtra[i].z;
    float visibleDepth = 2.0 * far * near / (far + near - (stored * 2.0 - 1.0) * (far - near));
    // Compare at the depth texel's ray, not this fragment's: a fixed bias against optical.z stripes sloping surfaces.
    vec2 depthSize = vec2(textureSize(depthMap, 0));
    vec2 texelCenter = (floor(depthUV * depthSize) + 0.5) / depthSize;
    vec3 depthRay = vec3((texelCenter - 0.5) * 2.0 * uSize[i].zw, 1.0);
    float denominator = dot(planeNormal, depthRay);
    float expectedDepth = abs(denominator) > 1e-12 ? dot(planeNormal, optical) / denominator : optical.z;
    float tolerance = max(0.003, min(0.02, expectedDepth * 0.002));
    if (expectedDepth <= 0.01 || abs(expectedDepth - visibleDepth) > tolerance) return;
    vec3 cameraNormal = normalize(planeNormal);
    float quality = abs(dot(cameraNormal, normalize(-optical))) * min(uLens[i].x, uLens[i].y) / optical.z;
    if (quality > bestQuality) { bestQuality = quality; projected = texture(camera, uv).rgb; }
}
void main() {
    if (uCount > 0) project(0, uCamera0, uDepth0);
    if (uCount > 1) project(1, uCamera1, uDepth1);
    if (uCount > 2) project(2, uCamera2, uDepth2);
    if (uCount > 3) project(3, uCamera3, uDepth3);
    fragColor = vec4(bestQuality > 0.0 ? projected : uColor * vLight, 1.0);
}"""

    const val DEPTH_VERTEX = """#version 300 es
uniform mat4 uMVP; in vec3 aPosition; void main() { gl_Position = uMVP * vec4(aPosition, 1.0); }"""
    const val DEPTH_FRAGMENT = """#version 300 es
precision mediump float; out vec4 color; void main() { color = vec4(0.0); }"""
}

/**
 * GPU state for projecting cameras: one depth map rendered from each camera's pose and its colour picture.
 * Runs on the GL thread only.
 */
internal class SiteProjectionGL {
    class Slot(var key: String = "", var depthTexture: Int = 0, var framebuffer: Int = 0, var colorTexture: Int = 0, var received: Long = -1,
                       var near: Float = 0.01f, var far: Float = 1f, var extent: FloatArray = floatArrayOf(1f, 1f), var worldToCamera: FloatArray = FloatArray(16), var mountingWall: Boolean = false)
    private val slots = HashMap<String, Slot>()
    var program = 0; private set
    private var depthProgram = 0
    var available = false; private set
    /** Camera id to its status for this frame. */
    @Volatile var statuses: Map<String, String> = emptyMap(); private set

    fun create(link: (String, String) -> Int) {
        slots.clear()
        available = GLES20.glGetString(GLES20.GL_VERSION)?.startsWith("OpenGL ES 3") == true
        if (!available) return
        program = link(SiteProjection.VERTEX, SiteProjection.FRAGMENT); depthProgram = link(SiteProjection.DEPTH_VERTEX, SiteProjection.DEPTH_FRAGMENT)
        val linked = IntArray(1)
        for (p in listOf(program, depthProgram)) { GLES20.glGetProgramiv(p, GLES20.GL_LINK_STATUS, linked, 0); if (linked[0] == 0) available = false }
    }

    fun invalidate() { slots.values.forEach { it.key = "" } }

    private fun release(slot: Slot) {
        GLES20.glDeleteTextures(2, intArrayOf(slot.depthTexture, slot.colorTexture), 0); GLES20.glDeleteFramebuffers(1, intArrayOf(slot.framebuffer), 0)
    }

    /** Prepares up to four cameras; returns those ready to draw, in order. */
    fun prepare(projectors: List<SiteProjector>, geometry: SiteGeometry?, draw: (Int) -> Unit, width: Int, height: Int): List<Pair<SiteProjector, Slot>> {
        val wanted = projectors.take(SiteProjection.SLOTS)
        slots.keys.filter { id -> wanted.none { it.id == id } }.forEach { slots.remove(it)?.let(::release) }
        if (!available || geometry == null) { statuses = wanted.associate { it.id to "Camera color needs OpenGL ES 3." }; return emptyList() }
        val ready = ArrayList<Pair<SiteProjector, Slot>>(); val nextStatuses = HashMap<String, String>()
        for (projector in wanted) {
            val lens = projector.geometry.lens; val extent = lens?.let(SiteProjection::extent)
            if (lens == null || extent == null) { nextStatuses[projector.id] = "Calibrate and align this camera to add color."; continue }
            val slot = slots.getOrPut(projector.id) {
                val textures = IntArray(2); GLES20.glGenTextures(2, textures, 0); val fb = IntArray(1); GLES20.glGenFramebuffers(1, fb, 0)
                Slot(depthTexture = textures[0], colorTexture = textures[1], framebuffer = fb[0])
            }
            val pose = projector.geometry.pose
            val key = "${System.identityHashCode(geometry)}:${pose.position}:${pose.rotation}:${lens.fx},${lens.fy},${lens.cx},${lens.cy},${lens.width}x${lens.height}"
            if (slot.key != key) {
                slot.extent = extent; slot.near = 0.01f; slot.far = SiteProjection.far(geometry, pose); slot.worldToCamera = SiteProjection.worldToCamera(pose)
                val excluded = SiteProjection.mountingWalls(geometry, pose); slot.mountingWall = excluded.isNotEmpty()
                renderDepth(slot, excluded, draw)
                GLES20.glViewport(0, 0, width, height)
                slot.key = key
            }
            if (slot.received != projector.received) {
                GLES20.glBindTexture(GLES20.GL_TEXTURE_2D, slot.colorTexture)
                for ((name, value) in listOf(GLES20.GL_TEXTURE_MIN_FILTER to GLES20.GL_LINEAR, GLES20.GL_TEXTURE_MAG_FILTER to GLES20.GL_LINEAR,
                    GLES20.GL_TEXTURE_WRAP_S to GLES20.GL_CLAMP_TO_EDGE, GLES20.GL_TEXTURE_WRAP_T to GLES20.GL_CLAMP_TO_EDGE)) GLES20.glTexParameteri(GLES20.GL_TEXTURE_2D, name, value)
                GLUtils.texImage2D(GLES20.GL_TEXTURE_2D, 0, projector.image, 0)
                slot.received = projector.received
            }
            nextStatuses[projector.id] = SiteProjection.status(projector.geometry, slot.mountingWall)
            ready += projector to slot
        }
        statuses = nextStatuses
        return ready
    }

    private fun renderDepth(slot: Slot, excluded: Set<Int>, draw: (Int) -> Unit) {
        GLES20.glBindTexture(GLES20.GL_TEXTURE_2D, slot.depthTexture)
        GLES30.glTexImage2D(GLES20.GL_TEXTURE_2D, 0, GLES30.GL_DEPTH_COMPONENT24, SiteProjection.DEPTH_SIZE, SiteProjection.DEPTH_SIZE, 0, GLES20.GL_DEPTH_COMPONENT, GLES20.GL_UNSIGNED_INT, null)
        for ((name, value) in listOf(GLES20.GL_TEXTURE_MIN_FILTER to GLES20.GL_NEAREST, GLES20.GL_TEXTURE_MAG_FILTER to GLES20.GL_NEAREST,
            GLES20.GL_TEXTURE_WRAP_S to GLES20.GL_CLAMP_TO_EDGE, GLES20.GL_TEXTURE_WRAP_T to GLES20.GL_CLAMP_TO_EDGE)) GLES20.glTexParameteri(GLES20.GL_TEXTURE_2D, name, value)
        GLES20.glBindFramebuffer(GLES20.GL_FRAMEBUFFER, slot.framebuffer)
        GLES20.glFramebufferTexture2D(GLES20.GL_FRAMEBUFFER, GLES20.GL_DEPTH_ATTACHMENT, GLES20.GL_TEXTURE_2D, slot.depthTexture, 0)
        GLES30.glDrawBuffers(1, intArrayOf(GLES20.GL_NONE), 0); GLES30.glReadBuffer(GLES20.GL_NONE)
        GLES20.glViewport(0, 0, SiteProjection.DEPTH_SIZE, SiteProjection.DEPTH_SIZE)
        GLES20.glClearDepthf(1f); GLES20.glClear(GLES20.GL_DEPTH_BUFFER_BIT)
        val mvp = FloatArray(16); Matrix.multiplyMM(mvp, 0, SiteProjection.depthProjection(slot.extent[0], slot.extent[1], slot.near, slot.far), 0, slot.worldToCamera, 0)
        GLES20.glUseProgram(depthProgram)
        GLES20.glUniformMatrix4fv(GLES20.glGetUniformLocation(depthProgram, "uMVP"), 1, false, mvp, 0)
        val position = GLES20.glGetAttribLocation(depthProgram, "aPosition")
        GLES20.glEnableVertexAttribArray(position)
        // The callback binds each mesh's buffer and points aPosition at it, skipping the mounting wall.
        excludedMeshes = excluded; depthPosition = position
        draw(depthProgram)
        GLES20.glDisableVertexAttribArray(position)
        GLES20.glBindFramebuffer(GLES20.GL_FRAMEBUFFER, 0)
    }
    var excludedMeshes: Set<Int> = emptySet(); private set
    var depthPosition = -1; private set

    /** Binds the projection uniforms and textures for [ready] on [program]. */
    fun bind(ready: List<Pair<SiteProjector, Slot>>) {
        GLES20.glUniform1i(GLES20.glGetUniformLocation(program, "uCount"), ready.size)
        ready.forEachIndexed { i, (projector, slot) ->
            val lens = projector.geometry.lens!!
            val d = if (lens.distortionModel == "brown_conrady") lens.distortion else List(5) { 0.0 }
            GLES20.glUniformMatrix4fv(GLES20.glGetUniformLocation(program, "uWorldToCamera[$i]"), 1, false, slot.worldToCamera, 0)
            GLES20.glUniform4f(GLES20.glGetUniformLocation(program, "uLens[$i]"), lens.fx.toFloat(), lens.fy.toFloat(), lens.cx.toFloat(), lens.cy.toFloat())
            GLES20.glUniform4f(GLES20.glGetUniformLocation(program, "uSize[$i]"), lens.width.toFloat(), lens.height.toFloat(), slot.extent[0], slot.extent[1])
            GLES20.glUniform4f(GLES20.glGetUniformLocation(program, "uDistortion[$i]"), d[0].toFloat(), d[1].toFloat(), d[2].toFloat(), d[3].toFloat())
            GLES20.glUniform4f(GLES20.glGetUniformLocation(program, "uExtra[$i]"), d[4].toFloat(), slot.near, slot.far, 0f)
            GLES20.glActiveTexture(GLES20.GL_TEXTURE0 + i * 2); GLES20.glBindTexture(GLES20.GL_TEXTURE_2D, slot.colorTexture)
            GLES20.glUniform1i(GLES20.glGetUniformLocation(program, "uCamera$i"), i * 2)
            GLES20.glActiveTexture(GLES20.GL_TEXTURE0 + i * 2 + 1); GLES20.glBindTexture(GLES20.GL_TEXTURE_2D, slot.depthTexture)
            GLES20.glUniform1i(GLES20.glGetUniformLocation(program, "uDepth$i"), i * 2 + 1)
        }
        // Unused slots still need valid sampler units.
        for (i in ready.size until SiteProjection.SLOTS) {
            GLES20.glUniform1i(GLES20.glGetUniformLocation(program, "uCamera$i"), 0); GLES20.glUniform1i(GLES20.glGetUniformLocation(program, "uDepth$i"), 1)
        }
        GLES20.glActiveTexture(GLES20.GL_TEXTURE0)
    }
}
