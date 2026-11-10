package dev.wendy.android

import java.io.ByteArrayInputStream
import java.nio.ByteBuffer
import java.nio.ByteOrder
import java.util.zip.ZipInputStream
import kotlin.math.abs
import kotlin.math.cos
import kotlin.math.sin

// A small, bounded reader for the USDZ site models iOS saves (RoomPlan, SceneKit and photo captures).
// It reads geometry only: meshes and cubes, their transforms, display or preview-surface colors, the
// layer's metersPerUnit and upAxis. It never follows references outside the archive or runs scripts.

class UsdFormatException(message: String) : Exception(message)

/** One mesh, triangulated, in metres with Y up. Positions and normals are xyz per vertex, three vertices per triangle. */
class SiteMesh(val name: String, val positions: FloatArray, val normals: FloatArray, val color: FloatArray)

class SiteGeometry(val meshes: List<SiteMesh>) {
    val triangles get() = meshes.sumOf { it.positions.size / 9 }
    val min = FloatArray(3) { Float.MAX_VALUE }
    val max = FloatArray(3) { -Float.MAX_VALUE }
    init {
        meshes.forEach { m -> for (i in m.positions.indices) { val axis = i % 3; min[axis] = minOf(min[axis], m.positions[i]); max[axis] = maxOf(max[axis], m.positions[i]) } }
        if (meshes.isEmpty() || meshes.all { it.positions.isEmpty() }) { min.fill(0f); max.fill(0f) }
    }
    val center get() = FloatArray(3) { (min[it] + max[it]) / 2 }
    val radius get() = maxOf(0.5f, kotlin.math.sqrt((0..2).sumOf { ((max[it] - min[it]) / 2.0).let { d -> d * d } }).toFloat())
}

/** The parsed layer: prims by path, in file order, with their attribute values and relationship targets. */
internal class UsdPrim(val path: String, var type: String = "") {
    val attributes = LinkedHashMap<String, Any?>()
    val relationships = LinkedHashMap<String, List<String>>()
    val children = mutableListOf<String>()
}
internal class UsdLayer(val prims: LinkedHashMap<String, UsdPrim>, val upAxis: String, val metersPerUnit: Double)

object UsdReader {
    const val MAXIMUM_BYTES = 128 * 1024 * 1024
    private const val MAXIMUM_TRIANGLES = 2_000_000

    /** Reads a USDZ archive (or a bare usda or usdc layer) into triangles in metres, Y up. */
    fun read(bytes: ByteArray): SiteGeometry {
        if (bytes.size > MAXIMUM_BYTES) throw UsdFormatException("This model is too large. Export a USDZ model smaller than 128 MB.")
        val layer = layerBytes(bytes)
        return compose(parseLayer(layer))
    }

    internal fun parseLayer(bytes: ByteArray): UsdLayer = when {
        bytes.size >= 8 && String(bytes, 0, 8, Charsets.US_ASCII) == "PXR-USDC" -> CrateReader(bytes).layer()
        String(bytes, 0, minOf(bytes.size, 6), Charsets.US_ASCII).startsWith("#usda") -> UsdaReader(String(bytes, Charsets.UTF_8)).layer()
        else -> throw UsdFormatException("Choose a self-contained USDZ model of the site.")
    }

    /** USDZ's first file is its default layer (USDZ specification). */
    private fun layerBytes(bytes: ByteArray): ByteArray {
        if (bytes.size < 4 || bytes[0] != 0x50.toByte() || bytes[1] != 0x4b.toByte()) return bytes
        ZipInputStream(ByteArrayInputStream(bytes)).use { zip ->
            while (true) {
                val entry = zip.nextEntry ?: break
                val name = entry.name.lowercase()
                if (entry.isDirectory || name.contains("..")) continue
                if (name.endsWith(".usdc") || name.endsWith(".usda") || name.endsWith(".usd")) {
                    val out = java.io.ByteArrayOutputStream()
                    val buffer = ByteArray(64 * 1024)
                    while (true) { val n = zip.read(buffer); if (n < 0) break; out.write(buffer, 0, n); if (out.size() > MAXIMUM_BYTES) throw UsdFormatException("This model is too large.") }
                    return out.toByteArray()
                }
            }
        }
        throw UsdFormatException("This USDZ file has no USD layer.")
    }

    // MARK: Composition

    private fun compose(layer: UsdLayer): SiteGeometry {
        val meshes = mutableListOf<SiteMesh>()
        var triangles = 0
        val scale = if (layer.metersPerUnit.isFinite() && layer.metersPerUnit > 0) layer.metersPerUnit else 1.0
        val zUp = layer.upAxis.equals("Z", ignoreCase = true)
        fun visit(path: String, parent: DoubleArray) {
            val prim = layer.prims[path] ?: return
            val (local, reset) = localTransform(prim)
            val world = if (reset) local else multiply(local, parent)
            when (prim.type) {
                "Mesh" -> mesh(prim, world, layer, scale, zUp)?.let { triangles += it.positions.size / 9; meshes += it }
                "Cube" -> cube(prim, world, layer, scale, zUp)?.let { triangles += it.positions.size / 9; meshes += it }
            }
            if (triangles > MAXIMUM_TRIANGLES) throw UsdFormatException("This model has too many triangles to show.")
            prim.children.forEach { visit(it, world) }
        }
        val roots = layer.prims["/"]?.children ?: layer.prims.keys.filter { it.count { c -> c == '/' } == 1 && it != "/" }
        roots.forEach { visit(it, identity()) }
        return SiteGeometry(meshes)
    }

    private fun mesh(prim: UsdPrim, world: DoubleArray, layer: UsdLayer, scale: Double, zUp: Boolean): SiteMesh? {
        val points = (prim.attributes["points"] as? FloatArray) ?: (prim.attributes["points"] as? DoubleArray)?.let { d -> FloatArray(d.size) { d[it].toFloat() } } ?: return null
        val indices = prim.attributes["faceVertexIndices"] as? IntArray ?: return null
        val counts = prim.attributes["faceVertexCounts"] as? IntArray ?: return null
        val vertexCount = points.size / 3
        val out = ArrayList<Float>(indices.size * 3)
        var cursor = 0
        for (count in counts) {
            if (count < 3 || cursor + count > indices.size) { cursor += maxOf(count, 0); continue }
            for (k in 1 until count - 1) for (corner in intArrayOf(indices[cursor], indices[cursor + k], indices[cursor + k + 1])) {
                if (corner !in 0 until vertexCount) throw UsdFormatException("A mesh in this model is damaged.")
                val p = transform(points[corner * 3].toDouble(), points[corner * 3 + 1].toDouble(), points[corner * 3 + 2].toDouble(), world, scale, zUp)
                out += p[0]; out += p[1]; out += p[2]
            }
            cursor += count
        }
        if (out.isEmpty()) return null
        return finish(prim, out.toFloatArray(), layer)
    }

    private fun cube(prim: UsdPrim, world: DoubleArray, layer: UsdLayer, scale: Double, zUp: Boolean): SiteMesh {
        val half = (number(prim.attributes["size"]) ?: 2.0) / 2
        val c = arrayOf(doubleArrayOf(-half, -half, -half), doubleArrayOf(half, -half, -half), doubleArrayOf(half, half, -half), doubleArrayOf(-half, half, -half),
            doubleArrayOf(-half, -half, half), doubleArrayOf(half, -half, half), doubleArrayOf(half, half, half), doubleArrayOf(-half, half, half))
        val faces = arrayOf(intArrayOf(0, 3, 2, 1), intArrayOf(4, 5, 6, 7), intArrayOf(0, 1, 5, 4), intArrayOf(2, 3, 7, 6), intArrayOf(1, 2, 6, 5), intArrayOf(0, 4, 7, 3))
        val out = ArrayList<Float>(108)
        for (f in faces) for (corner in intArrayOf(f[0], f[1], f[2], f[0], f[2], f[3])) {
            val p = transform(c[corner][0], c[corner][1], c[corner][2], world, scale, zUp); out += p[0]; out += p[1]; out += p[2]
        }
        return finish(prim, out.toFloatArray(), layer)
    }

    private fun finish(prim: UsdPrim, positions: FloatArray, layer: UsdLayer): SiteMesh {
        val normals = FloatArray(positions.size)
        var i = 0
        while (i + 8 < positions.size) {
            val ax = positions[i + 3] - positions[i]; val ay = positions[i + 4] - positions[i + 1]; val az = positions[i + 5] - positions[i + 2]
            val bx = positions[i + 6] - positions[i]; val by = positions[i + 7] - positions[i + 1]; val bz = positions[i + 8] - positions[i + 2]
            var nx = ay * bz - az * by; var ny = az * bx - ax * bz; var nz = ax * by - ay * bx
            val length = kotlin.math.sqrt(nx * nx + ny * ny + nz * nz)
            if (length > 1e-12f) { nx /= length; ny /= length; nz /= length } else { nx = 0f; ny = 1f; nz = 0f }
            for (v in 0 until 3) { normals[i + v * 3] = nx; normals[i + v * 3 + 1] = ny; normals[i + v * 3 + 2] = nz }
            i += 9
        }
        return SiteMesh(prim.path.substringAfterLast('/'), positions, normals, color(prim, layer))
    }

    private fun color(prim: UsdPrim, layer: UsdLayer): FloatArray {
        (prim.attributes["primvars:displayColor"] as? FloatArray)?.takeIf { it.size >= 3 }?.let { return floatArrayOf(it[0], it[1], it[2]) }
        // UsdPreviewSurface diffuseColor of the bound material.
        val material = prim.relationships["material:binding"]?.firstOrNull()?.let { layer.prims[it] }
        material?.children?.mapNotNull { layer.prims[it] }?.forEach { shader ->
            (shader.attributes["inputs:diffuseColor"] as? FloatArray)?.takeIf { it.size >= 3 }?.let { return floatArrayOf(it[0], it[1], it[2]) }
        }
        return floatArrayOf(0.78f, 0.78f, 0.76f)
    }

    private fun transform(x: Double, y: Double, z: Double, m: DoubleArray, scale: Double, zUp: Boolean): FloatArray {
        val tx = (x * m[0] + y * m[4] + z * m[8] + m[12]) * scale
        val ty = (x * m[1] + y * m[5] + z * m[9] + m[13]) * scale
        val tz = (x * m[2] + y * m[6] + z * m[10] + m[14]) * scale
        return if (zUp) floatArrayOf(tx.toFloat(), tz.toFloat(), (-ty).toFloat()) else floatArrayOf(tx.toFloat(), ty.toFloat(), tz.toFloat())
    }

    /** The prim's local matrix from xformOpOrder, in USD's row-vector convention; true when it resets the parent stack. */
    private fun localTransform(prim: UsdPrim): Pair<DoubleArray, Boolean> {
        val order = prim.attributes["xformOpOrder"] as? List<*> ?: return identity() to false
        var result = identity()
        var reset = false
        for (raw in order) {
            val name = raw as? String ?: continue
            if (name == "!resetXformStack!") { reset = true; result = identity(); continue }
            if (name.startsWith("!invert!")) continue
            val value = prim.attributes[name]
            val op = when {
                name.startsWith("xformOp:transform") -> (value as? DoubleArray)?.takeIf { it.size == 16 }
                name.startsWith("xformOp:translate") -> vector(value)?.let { translate(it) }
                name.startsWith("xformOp:scale") -> vector(value)?.let { scaleMatrix(it) }
                name.startsWith("xformOp:orient") -> vector(value)?.takeIf { it.size == 4 }?.let { quaternion(it) }
                name.startsWith("xformOp:rotateXYZ") -> vector(value)?.let { multiply(multiply(rotate(0, it[0]), rotate(1, it[1])), rotate(2, it[2])) }
                name.startsWith("xformOp:rotateZYX") -> vector(value)?.let { multiply(multiply(rotate(2, it[2]), rotate(1, it[1])), rotate(0, it[0])) }
                name.startsWith("xformOp:rotateX") -> number(value)?.let { rotate(0, it) }
                name.startsWith("xformOp:rotateY") -> number(value)?.let { rotate(1, it) }
                name.startsWith("xformOp:rotateZ") -> number(value)?.let { rotate(2, it) }
                else -> null
            } ?: continue
            // The first op is outermost: points are transformed by the last op first.
            result = multiply(op, result)
        }
        return result to reset
    }

    private fun number(value: Any?): Double? = when (value) { is Number -> value.toDouble(); is FloatArray -> value.firstOrNull()?.toDouble(); is DoubleArray -> value.firstOrNull(); else -> null }
    private fun vector(value: Any?): DoubleArray? = when (value) {
        is DoubleArray -> value
        is FloatArray -> DoubleArray(value.size) { value[it].toDouble() }
        is IntArray -> DoubleArray(value.size) { value[it].toDouble() }
        else -> null
    }
    internal fun identity() = DoubleArray(16).also { it[0] = 1.0; it[5] = 1.0; it[10] = 1.0; it[15] = 1.0 }
    internal fun multiply(a: DoubleArray, b: DoubleArray) = DoubleArray(16) { i -> val r = i / 4; val c = i % 4; (0..3).sumOf { a[r * 4 + it] * b[it * 4 + c] } }
    private fun translate(v: DoubleArray) = identity().also { it[12] = v[0]; it[13] = v.getOrElse(1) { 0.0 }; it[14] = v.getOrElse(2) { 0.0 } }
    private fun scaleMatrix(v: DoubleArray) = identity().also { it[0] = v[0]; it[5] = v.getOrElse(1) { v[0] }; it[10] = v.getOrElse(2) { v[0] } }
    private fun rotate(axis: Int, degrees: Double): DoubleArray {
        val r = Math.toRadians(degrees); val c = cos(r); val s = sin(r)
        return identity().also { m ->
            when (axis) {
                0 -> { m[5] = c; m[6] = s; m[9] = -s; m[10] = c }
                1 -> { m[0] = c; m[2] = -s; m[8] = s; m[10] = c }
                else -> { m[0] = c; m[1] = s; m[4] = -s; m[5] = c }
            }
        }
    }
    /** GfQuat stores the imaginary part first, then the real part. */
    private fun quaternion(q: DoubleArray): DoubleArray {
        val x = q[0]; val y = q[1]; val z = q[2]; val w = q[3]
        val n = kotlin.math.sqrt(x * x + y * y + z * z + w * w).takeIf { it > 1e-12 } ?: return identity()
        val (qx, qy, qz, qw) = listOf(x / n, y / n, z / n, w / n)
        return doubleArrayOf(
            1 - 2 * (qy * qy + qz * qz), 2 * (qx * qy + qz * qw), 2 * (qx * qz - qy * qw), 0.0,
            2 * (qx * qy - qz * qw), 1 - 2 * (qx * qx + qz * qz), 2 * (qy * qz + qx * qw), 0.0,
            2 * (qx * qz + qy * qw), 2 * (qy * qz - qx * qw), 1 - 2 * (qx * qx + qy * qy), 0.0,
            0.0, 0.0, 0.0, 1.0)
    }
}

// MARK: Binary crate layers (usdc)

/** USD's crate format: bootstrap, table of contents, compressed token, field, path and spec tables. */
private class CrateReader(private val data: ByteArray) {
    private val buffer = ByteBuffer.wrap(data).order(ByteOrder.LITTLE_ENDIAN)
    private val version = (data[8].toInt() and 0xFF) * 10_000 + (data[9].toInt() and 0xFF) * 100 + (data[10].toInt() and 0xFF)
    private val sections = HashMap<String, Pair<Long, Long>>()
    private lateinit var tokens: Array<String>
    private lateinit var strings: IntArray
    private lateinit var fieldTokens: IntArray
    private lateinit var fieldReps: LongArray
    private lateinit var fieldSets: IntArray
    private lateinit var paths: Array<String?>
    private val specPaths = mutableListOf<Int>()
    private val specFieldSets = mutableListOf<Int>()
    private val specTypes = mutableListOf<Int>()

    init {
        if (version < 400) throw UsdFormatException("This USD file uses an old format that Wendy can’t read. Export it again.")
        val toc = buffer.getLong(16)
        check(toc in 88 until data.size) { "Invalid USD table of contents" }
        var p = toc.toInt()
        val count = buffer.getLong(p); p += 8
        if (count !in 1..64) throw UsdFormatException("Invalid USD table of contents")
        repeat(count.toInt()) {
            val name = String(data, p, 16, Charsets.US_ASCII).trimEnd('\u0000'); p += 16
            val start = buffer.getLong(p); val size = buffer.getLong(p + 8); p += 16
            if (start < 0 || size < 0 || start + size > data.size) throw UsdFormatException("Invalid USD section")
            sections[name] = start to size
        }
        readTokens(); readStrings(); readFields(); readFieldSets(); readPaths(); readSpecs()
    }

    private fun section(name: String): Int = sections[name]?.first?.toInt() ?: throw UsdFormatException("Missing USD section $name")

    private fun readTokens() {
        var p = section("TOKENS")
        val count = buffer.getLong(p).toInt(); val uncompressed = buffer.getLong(p + 8).toInt(); val compressed = buffer.getLong(p + 16).toInt(); p += 24
        if (count < 0 || uncompressed !in 0..(64 shl 20)) throw UsdFormatException("Invalid USD tokens")
        val raw = Lz4.fast(data, p, compressed, uncompressed)
        val list = ArrayList<String>(count)
        var start = 0
        for (i in raw.indices) if (raw[i].toInt() == 0) { list += String(raw, start, i - start, Charsets.UTF_8); start = i + 1; if (list.size == count) break }
        if (list.size != count) throw UsdFormatException("Invalid USD tokens")
        tokens = list.toTypedArray()
    }
    private fun readStrings() {
        val p = section("STRINGS")
        val count = buffer.getLong(p).toInt()
        strings = IntArray(count) { buffer.getInt(p + 8 + it * 4) }
    }
    private fun readFields() {
        var p = section("FIELDS")
        val count = buffer.getLong(p).toInt(); p += 8
        val (indexes, next) = compressedInts(p, count); fieldTokens = indexes; p = next
        val compressed = buffer.getLong(p).toInt(); p += 8
        val raw = Lz4.fast(data, p, compressed, count * 8)
        val reps = ByteBuffer.wrap(raw).order(ByteOrder.LITTLE_ENDIAN)
        fieldReps = LongArray(count) { reps.getLong(it * 8) }
    }
    private fun readFieldSets() {
        val p = section("FIELDSETS")
        val count = buffer.getLong(p).toInt()
        fieldSets = compressedInts(p + 8, count).first
    }
    private fun readPaths() {
        var p = section("PATHS")
        val total = buffer.getLong(p).toInt(); p += 8
        val encoded = buffer.getLong(p).toInt(); p += 8
        val (pathIndexes, a) = compressedInts(p, encoded)
        val (elementTokens, b) = compressedInts(a, encoded)
        val (jumps, _) = compressedInts(b, encoded)
        paths = arrayOfNulls(total)
        if (encoded > 0) build(pathIndexes, elementTokens, jumps, 0, null)
    }
    /** Rebuilds paths from their compressed tree: a jump to the next sibling, -1 for a child only, -2 for a leaf. */
    private fun build(pathIndexes: IntArray, elementTokens: IntArray, jumps: IntArray, start: Int, initialParent: String?) {
        // An explicit stack replaces USD's recursion so deep or wide trees cannot overflow.
        val pending = ArrayDeque<Pair<Int, String?>>()
        pending.addLast(start to initialParent)
        while (pending.isNotEmpty()) {
            var (current, parent) = pending.removeLast()
            while (true) {
                val index = current++
                if (index !in pathIndexes.indices) throw UsdFormatException("Invalid USD paths")
                val path = if (parent == null) "/" else {
                    val token = elementTokens[index]
                    val name = tokens.getOrNull(abs(token)) ?: throw UsdFormatException("Invalid USD paths")
                    if (token < 0) "$parent.$name" else if (parent == "/") "/$name" else "$parent/$name"
                }
                paths[pathIndexes[index]] = path
                val jump = jumps[index]
                val hasChild = jump > 0 || jump == -1
                val hasSibling = jump >= 0
                if (hasChild) {
                    if (hasSibling) pending.addLast(index + jump to parent)
                    parent = path
                } else if (!hasSibling) break
            }
        }
    }
    private fun readSpecs() {
        var p = section("SPECS")
        val count = buffer.getLong(p).toInt(); p += 8
        val (pathIndexes, a) = compressedInts(p, count)
        val (fieldSetIndexes, b) = compressedInts(a, count)
        val (types, _) = compressedInts(b, count)
        pathIndexes.forEach { specPaths += it }; fieldSetIndexes.forEach { specFieldSets += it }; types.forEach { specTypes += it }
    }

    /** A compressed run of 32-bit integers: its byte size, then LZ4 holding USD's delta encoding. */
    private fun compressedInts(at: Int, count: Int): Pair<IntArray, Int> {
        val size = buffer.getLong(at).toInt()
        if (count == 0) return IntArray(0) to at + 8 + size
        val working = 4 + (count * 2 + 7) / 8 + count * 4
        val raw = Lz4.fast(data, at + 8, size, working)
        return UsdIntegers.decode32(raw, count) to at + 8 + size
    }

    fun layer(): UsdLayer {
        val prims = LinkedHashMap<String, UsdPrim>()
        var upAxis = "Y"; var metersPerUnit = 0.01
        var sawMetres = false
        for (i in specPaths.indices) {
            val path = paths.getOrNull(specPaths[i]) ?: continue
            val fields = fieldsFor(specFieldSets[i])
            when (specTypes[i]) {
                7 -> { // Pseudo-root: layer metadata and root prims.
                    fields["upAxis"]?.let { (value(it) as? String)?.let { a -> upAxis = a } }
                    fields["metersPerUnit"]?.let { (value(it) as? Number)?.let { m -> metersPerUnit = m.toDouble(); sawMetres = true } }
                    prims.getOrPut("/") { UsdPrim("/") }.children += tokenList(fields["primChildren"]).map { "/$it" }
                }
                6 -> {
                    val prim = prims.getOrPut(path) { UsdPrim(path) }
                    prim.type = fields["typeName"]?.let { value(it) as? String } ?: prim.type
                    prim.children += tokenList(fields["primChildren"]).map { "$path/$it" }
                }
                1 -> {
                    val owner = path.substringBeforeLast('.')
                    val name = path.substringAfterLast('.')
                    val prim = prims.getOrPut(owner) { UsdPrim(owner) }
                    fields["default"]?.let { prim.attributes[name] = value(it) }
                }
                8 -> {
                    val owner = path.substringBeforeLast('.')
                    val prim = prims.getOrPut(owner) { UsdPrim(owner) }
                    fields["targetPaths"]?.let { prim.relationships[path.substringAfterLast('.')] = pathList(it) }
                }
            }
        }
        // USD's fallback is centimetres when a layer omits metersPerUnit.
        return UsdLayer(prims, upAxis, if (sawMetres) metersPerUnit else 0.01)
    }

    private fun fieldsFor(set: Int): Map<String, Long> {
        val result = HashMap<String, Long>()
        var i = set
        while (i in fieldSets.indices && fieldSets[i] != -1) {
            val field = fieldSets[i]
            if (field in fieldTokens.indices) tokens.getOrNull(fieldTokens[field])?.let { result[it] = fieldReps[field] }
            i++
        }
        return result
    }

    private fun tokenList(rep: Long?): List<String> {
        rep ?: return emptyList()
        val v = value(rep)
        @Suppress("UNCHECKED_CAST")
        return (v as? List<String>).orEmpty()
    }

    private fun pathList(rep: Long): List<String> {
        val type = ((rep ushr 48) and 0xFF).toInt()
        if (type != 34) return emptyList()
        var p = (rep and PAYLOAD).toInt()
        val header = data[p].toInt() and 0xFF; p += 1
        val result = mutableListOf<String>()
        for (bit in intArrayOf(1, 2, 5, 6, 3, 4)) {
            if (header and (1 shl bit) == 0) continue
            val count = buffer.getLong(p).toInt(); p += 8
            repeat(count) { val index = buffer.getInt(p); p += 4; if (bit != 3) paths.getOrNull(index)?.let(result::add) }
        }
        return result
    }

    /** Decodes the value types site geometry uses; anything else reads as null. */
    private fun value(rep: Long): Any? {
        val array = rep < 0
        val inlined = rep and (1L shl 62) != 0L
        val compressed = rep and (1L shl 61) != 0L
        val type = ((rep ushr 48) and 0xFF).toInt()
        val payload = rep and PAYLOAD
        if (inlined) return when (type) {
            1, 2, 3, 4 -> payload.toInt()
            5, 6 -> payload
            8, 9 -> java.lang.Float.intBitsToFloat(payload.toInt()).toDouble()
            11 -> tokens.getOrNull(payload.toInt())
            10 -> strings.getOrNull(payload.toInt())?.let { tokens.getOrNull(it) }
            19, 20, 22, 23, 24, 26, 27, 28, 30 -> {
                val n = when (type) { 19, 20, 22 -> 2; 27, 28, 30 -> 4; else -> 3 }
                if (type == 24 || type == 20 || type == 28) FloatArray(n) { ((payload ushr (it * 8)) and 0xFF).toByte().toFloat() }
                else DoubleArray(n) { ((payload ushr (it * 8)) and 0xFF).toByte().toDouble() }
            }
            15 -> DoubleArray(16).also { m -> for (d in 0..3) m[d * 5] = ((payload ushr (d * 8)) and 0xFF).toByte().toDouble() }
            else -> null
        }
        var p = payload.toInt()
        if (p !in 0 until data.size) return null
        if (!array) return when (type) {
            9 -> buffer.getDouble(p)
            15 -> DoubleArray(16) { buffer.getDouble(p + it * 8) }
            16 -> DoubleArray(4) { buffer.getDouble(p + it * 8) }
            17 -> DoubleArray(4) { buffer.getFloat(p + it * 4).toDouble() }
            23 -> DoubleArray(3) { buffer.getDouble(p + it * 8) }
            24 -> FloatArray(3) { buffer.getFloat(p + it * 4) }
            27 -> DoubleArray(4) { buffer.getDouble(p + it * 8) }
            28 -> FloatArray(4) { buffer.getFloat(p + it * 4) }
            41 -> { val count = buffer.getLong(p).toInt(); List(count) { tokens.getOrNull(buffer.getInt(p + 8 + it * 4)) ?: "" } }
            else -> null
        }
        val count: Int
        if (version >= 700) { count = buffer.getLong(p).toInt(); p += 8 } else { count = buffer.getInt(p); p += 4 }
        if (count < 0 || count > 64_000_000) throw UsdFormatException("A value in this model is too large.")
        return when (type) {
            3, 4 -> if (compressed && version >= 500) compressedInts(p, count).first else IntArray(count) { buffer.getInt(p + it * 4) }
            8 -> if (compressed && version >= 600) floats(p, count) else FloatArray(count) { buffer.getFloat(p + it * 4) }
            9 -> if (compressed && version >= 600) floats(p, count, double = true) else DoubleArray(count) { buffer.getDouble(p + it * 8) }
            11 -> List(count) { tokens.getOrNull(buffer.getInt(p + it * 4)) ?: "" }
            20 -> FloatArray(count * 2) { buffer.getFloat(p + it * 4) }
            24 -> FloatArray(count * 3) { buffer.getFloat(p + it * 4) }
            23 -> DoubleArray(count * 3) { buffer.getDouble(p + it * 8) }
            28 -> FloatArray(count * 4) { buffer.getFloat(p + it * 4) }
            25 -> FloatArray(count * 3) { half(buffer.getShort(p + it * 2)) }
            else -> null
        }
    }

    /** Compressed float arrays are integers ('i') or a lookup table and indexes ('t'). */
    private fun floats(at: Int, count: Int, double: Boolean = false): Any {
        var p = at
        val code = data[p].toInt().toChar(); p += 1
        return when (code) {
            'i' -> { val ints = compressedInts(p, count).first; if (double) DoubleArray(count) { ints[it].toDouble() } else FloatArray(count) { ints[it].toFloat() } }
            't' -> {
                val size = buffer.getInt(p); p += 4
                val table = if (double) DoubleArray(size) { buffer.getDouble(p + it * 8) } else null
                val tableF = if (!double) FloatArray(size) { buffer.getFloat(p + it * 4) } else null
                p += size * if (double) 8 else 4
                val indexes = compressedInts(p, count).first
                if (double) DoubleArray(count) { table!![indexes[it]] } else FloatArray(count) { tableF!![indexes[it]] }
            }
            else -> throw UsdFormatException("Unsupported compressed values in this model.")
        }
    }

    private fun half(bits: Short): Float {
        val h = bits.toInt() and 0xFFFF
        val sign = if (h and 0x8000 != 0) -1f else 1f
        val exponent = (h shr 10) and 0x1F; val mantissa = h and 0x3FF
        return sign * when (exponent) { 0 -> mantissa / 1024f * 2f.pow(-14); 31 -> Float.POSITIVE_INFINITY; else -> (1 + mantissa / 1024f) * 2f.pow(exponent - 15) }
    }
    private fun Float.pow(e: Int) = Math.pow(this.toDouble(), e.toDouble()).toFloat()

    private companion object { const val PAYLOAD = (1L shl 48) - 1 }
}

/** USD integer compression: a common delta, two-bit codes, then 8, 16 or 32-bit deltas. */
internal object UsdIntegers {
    fun decode32(raw: ByteArray, count: Int): IntArray {
        val buffer = ByteBuffer.wrap(raw).order(ByteOrder.LITTLE_ENDIAN)
        val common = buffer.getInt(0)
        val codes = 4
        var values = codes + (count * 2 + 7) / 8
        val out = IntArray(count)
        var previous = 0
        for (i in 0 until count) {
            val code = (raw[codes + i / 4].toInt() shr ((i % 4) * 2)) and 3
            previous += when (code) {
                0 -> common
                1 -> raw[values].toInt().also { values += 1 }
                2 -> buffer.getShort(values).toInt().also { values += 2 }
                else -> buffer.getInt(values).also { values += 4 }
            }
            out[i] = previous
        }
        return out
    }
}

/** LZ4 block decompression inside TfFastCompression's chunked framing. */
internal object Lz4 {
    fun fast(source: ByteArray, offset: Int, size: Int, outputSize: Int): ByteArray {
        if (size <= 0 || offset < 0 || offset + size > source.size) throw UsdFormatException("Invalid compressed data in this model.")
        val output = ByteArray(outputSize)
        val chunks = source[offset].toInt() and 0xFF
        if (chunks == 0) { val n = block(source, offset + 1, size - 1, output, 0); return if (n == outputSize) output else output.copyOf(n) }
        var p = offset + 1; var written = 0
        repeat(chunks) {
            val chunk = ByteBuffer.wrap(source, p, 4).order(ByteOrder.LITTLE_ENDIAN).int; p += 4
            written += block(source, p, chunk, output, written); p += chunk
        }
        return if (written == outputSize) output else output.copyOf(written)
    }

    fun block(src: ByteArray, start: Int, length: Int, dst: ByteArray, dstStart: Int): Int {
        var s = start; val end = start + length; var d = dstStart
        while (s < end) {
            val token = src[s++].toInt() and 0xFF
            var literal = token ushr 4
            if (literal == 15) { while (true) { val b = src[s++].toInt() and 0xFF; literal += b; if (b != 255) break } }
            if (d + literal > dst.size || s + literal > end) throw UsdFormatException("Invalid compressed data in this model.")
            System.arraycopy(src, s, dst, d, literal); s += literal; d += literal
            if (s >= end) break
            val offset = (src[s].toInt() and 0xFF) or ((src[s + 1].toInt() and 0xFF) shl 8); s += 2
            if (offset == 0 || offset > d - dstStart) throw UsdFormatException("Invalid compressed data in this model.")
            var match = (token and 15) + 4
            if (match == 19) { while (true) { val b = src[s++].toInt() and 0xFF; match += b; if (b != 255) break } }
            if (d + match > dst.size) throw UsdFormatException("Invalid compressed data in this model.")
            var from = d - offset
            repeat(match) { dst[d++] = dst[from++] }
        }
        return d - dstStart
    }
}

// MARK: Text layers (usda)

/** A tolerant reader for the usda subset geometry needs: prims, typed attributes and relationships. */
private class UsdaReader(private val text: String) {
    private var i = 0
    private val prims = LinkedHashMap<String, UsdPrim>()
    private var upAxis = "Y"
    private var metersPerUnit = 0.01

    fun layer(): UsdLayer {
        skipLine() // #usda 1.0
        space()
        if (peek() == '(') layerMetadata()
        val root = UsdPrim("/").also { prims["/"] = it }
        while (true) { space(); if (i >= text.length) break; if (!primOrSkip("/", root)) i++ }
        return UsdLayer(prims, upAxis, metersPerUnit)
    }

    private fun layerMetadata() {
        val end = matching(i)
        val body = text.substring(i + 1, end)
        Regex("upAxis\\s*=\\s*\"(\\w+)\"").find(body)?.let { upAxis = it.groupValues[1] }
        Regex("metersPerUnit\\s*=\\s*([-+0-9.eE]+)").find(body)?.let { metersPerUnit = it.groupValues[1].toDoubleOrNull() ?: metersPerUnit }
        i = end + 1
    }

    /** Reads `def Type "name" (...) { ... }`; returns false when the next word is not a prim. */
    private fun primOrSkip(parentPath: String, parent: UsdPrim): Boolean {
        val save = i
        val word = word()
        if (word !in setOf("def", "over", "class")) { i = save; return false }
        space()
        val type = if (peek() == '"') "" else word()
        space()
        val name = quoted()
        val path = if (parentPath == "/") "/$name" else "$parentPath/$name"
        val prim = prims.getOrPut(path) { UsdPrim(path) }.also { if (type.isNotEmpty()) it.type = type }
        parent.children += path
        space()
        if (peek() == '(') i = matching(i) + 1
        space()
        if (peek() != '{') return true
        i++
        while (true) {
            space()
            if (i >= text.length) break
            if (peek() == '}') { i++; break }
            if (primOrSkip(path, prim)) continue
            val before = i
            statement(prim)
            if (i == before) i++
        }
        return true
    }

    /** One property line, or anything else up to the end of its line. */
    private fun statement(prim: UsdPrim) {
        val start = i
        val words = mutableListOf<String>()
        while (i < text.length && peek() != '=' && peek() != '\n' && peek() != '{' && peek() != '}' && peek() != '(') {
            space(sameLine = true); val w = word(); if (w.isEmpty()) break; words += w; space(sameLine = true)
        }
        if (i < text.length && peek() == '(' && words.isEmpty()) { i = matching(i) + 1; return }
        if (i < text.length && peek() == '{') { i = matching(i, '{', '}') + 1; return }
        if (i >= text.length || peek() != '=') { if (i == start) i++; skipLine(); return }
        i++ // '='
        space(sameLine = true)
        val name = words.lastOrNull() ?: run { skipLine(); return }
        val isRelationship = words.firstOrNull() == "rel"
        val type = words.dropLast(1).lastOrNull() ?: ""
        val value = value()
        if (isRelationship) {
            prim.relationships[name] = when (value) { is String -> listOf(value); is List<*> -> value.filterIsInstance<String>(); else -> emptyList() }
        } else prim.attributes[name.removeSuffix(".connect")] = typed(type, value)
        space(sameLine = true)
        if (peek() == '(') i = matching(i) + 1
    }

    private fun typed(type: String, value: Any?): Any? {
        val base = type.removeSuffix("[]")
        val array = type.endsWith("[]")
        fun flatten(v: Any?): List<Double> = when (v) { is Double -> listOf(v); is List<*> -> v.flatMap { flatten(it) }; else -> emptyList() }
        return when {
            base in setOf("int", "uint", "int64") && array -> flatten(value).map { it.toInt() }.toIntArray()
            base in setOf("int", "uint", "int64", "double", "float", "half") && !array -> (value as? Double)
            base.startsWith("matrix4d") -> flatten(value).takeIf { it.size == 16 }?.toDoubleArray()
            // usda writes quaternions real part first; the crate and GfQuat keep it last.
            base.startsWith("quat") && !array -> flatten(value).takeIf { it.size == 4 }?.let { q -> doubleArrayOf(q[1], q[2], q[3], q[0]) }
            base == "token" && array -> (value as? List<*>)?.filterIsInstance<String>()
            base == "token" || base == "string" -> value as? String
            base.startsWith("double") || base.startsWith("quatd") -> flatten(value).toDoubleArray()
            base.isNotEmpty() && (base[0].isLetter()) -> flatten(value).map { it.toFloat() }.toFloatArray().takeIf { it.isNotEmpty() } ?: value
            else -> value
        }
    }

    private fun value(): Any? {
        space()
        return when (peek()) {
            '[' -> list(']')
            '(' -> list(')')
            '"' -> quoted()
            '<' -> { val end = text.indexOf('>', i); val v = text.substring(i + 1, end); i = end + 1; v }
            '@' -> { val end = text.indexOf('@', i + 1); i = end + 1; null }
            else -> { val w = word(); w.toDoubleOrNull() ?: w }
        }
    }
    private fun list(close: Char): List<Any?> {
        i++
        val items = mutableListOf<Any?>()
        while (true) {
            space()
            if (i >= text.length) break
            if (peek() == close) { i++; break }
            if (peek() == ',') { i++; continue }
            val before = i
            items += value()
            // A damaged file must not stall the reader.
            if (i == before) i++
        }
        return items
    }

    private fun peek() = text[i]
    private fun space(sameLine: Boolean = false) {
        while (i < text.length) {
            val c = text[i]
            if (c == '#') { skipLine(); continue }
            if (c == ' ' || c == '\t' || c == '\r' || (!sameLine && c == '\n')) i++ else break
        }
    }
    private fun skipLine() { while (i < text.length && text[i] != '\n') i++; if (i < text.length) i++ }
    private fun word(): String {
        val start = i
        while (i < text.length) {
            val c = text[i]
            if (c.isLetterOrDigit() || c in "_:.-+!") i++
            else if (c == '[' && i + 1 < text.length && text[i + 1] == ']') i += 2
            else break
        }
        return text.substring(start, i)
    }
    private fun quoted(): String {
        if (i >= text.length || peek() != '"') return word()
        val triple = text.startsWith("\"\"\"", i)
        val close = if (triple) "\"\"\"" else "\""
        val start = i + close.length
        var end = text.indexOf(close, start)
        while (!triple && end > 0 && text[end - 1] == '\\') end = text.indexOf(close, end + 1)
        if (end < 0) end = text.length
        i = end + close.length
        return text.substring(start, end)
    }
    private fun matching(open: Int, a: Char = '(', b: Char = ')'): Int {
        var depth = 0; var k = open; var inString = false
        while (k < text.length) {
            val c = text[k]
            if (c == '"' && (k == 0 || text[k - 1] != '\\')) inString = !inString
            else if (!inString) { if (c == a) depth++ else if (c == b) { depth--; if (depth == 0) return k } }
            k++
        }
        return text.length - 1
    }
}
