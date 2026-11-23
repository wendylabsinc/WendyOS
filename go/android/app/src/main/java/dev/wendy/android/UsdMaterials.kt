package dev.wendy.android

import java.io.ByteArrayInputStream
import java.util.zip.ZipInputStream
import kotlin.math.cos
import kotlin.math.sin

/** Encoded image stays bounded and is decoded only by the render thread. USD UVs have a bottom-left origin. */
class SiteTexture(val name: String, val bytes: ByteArray, val wrapS: String = "repeat", val wrapT: String = "repeat",
                  val scale: FloatArray = floatArrayOf(1f, 1f, 1f, 1f), val bias: FloatArray = FloatArray(4))

internal class UsdAssets(val layer: ByteArray, val layerName: String, private val files: Map<String, ByteArray>) {
    fun image(path: String): ByteArray {
        val name = resolve(layerName.substringBeforeLast('/', ""), path)
        return files[name]?.takeIf { it.size <= 32 * 1024 * 1024 }
            ?: throw UsdFormatException("Missing or oversized embedded texture: $path")
    }
    companion object {
        fun resolve(base: String, path: String): String {
            if (path.isBlank() || path.startsWith('/') || ':' in path || '\\' in path || '\u0000' in path) throw UsdFormatException("Texture must be inside the USDZ archive")
            val parts = ArrayDeque<String>()
            for (part in (if (base.isEmpty()) path else "$base/$path").split('/')) when (part) {
                "", "." -> Unit
                ".." -> if (parts.isEmpty()) throw UsdFormatException("Texture leaves the USDZ archive") else parts.removeLast()
                else -> parts.addLast(part)
            }
            return parts.joinToString("/")
        }
        fun read(bytes: ByteArray): UsdAssets {
            if (bytes.size < 4 || bytes[0] != 0x50.toByte() || bytes[1] != 0x4b.toByte()) return UsdAssets(bytes, "model.usda", emptyMap())
            val files = LinkedHashMap<String, ByteArray>()
            var total = 0L
            ZipInputStream(ByteArrayInputStream(bytes)).use { zip ->
                while (true) {
                    val entry = zip.nextEntry ?: break
                    if (entry.isDirectory) continue
                    if (files.size >= 512) throw UsdFormatException("Too many files in this USDZ")
                    val name = resolve("", entry.name)
                    if (name in files) throw UsdFormatException("Duplicate USDZ entry")
                    val out = java.io.ByteArrayOutputStream()
                    val buffer = ByteArray(64 * 1024)
                    while (true) {
                        val n = zip.read(buffer); if (n < 0) break
                        total += n
                        if (total > UsdReader.MAXIMUM_BYTES) throw UsdFormatException("Expanded USDZ exceeds 128 MB")
                        out.write(buffer, 0, n)
                    }
                    files[name] = out.toByteArray()
                }
            }
            val first = files.entries.firstOrNull() ?: throw UsdFormatException("Empty USDZ")
            return UsdAssets(first.value, first.key, files)
        }
    }
}

internal class UsdMaterial(val color: FloatArray, val texture: SiteTexture?, private val uvName: String = "st",
                           private val uvScale: FloatArray = floatArrayOf(1f, 1f), private val uvTranslation: FloatArray = FloatArray(2), private val rotation: Float = 0f) {
    fun coordinates(prim: UsdPrim, face: Int, corner: Int, vertex: Int): FloatArray {
        val key = "primvars:$uvName"
        val values = floats(prim.attributes[key]) ?: throw UsdFormatException("Textured mesh has no $uvName coordinates")
        val interpolation = prim.attributes["$key:interpolation"] as? String ?: "constant"
        var index = when (interpolation) {
            "faceVarying" -> corner; "vertex", "varying" -> vertex; "uniform" -> face; "constant" -> 0
            else -> throw UsdFormatException("Unsupported texture interpolation: $interpolation")
        }
        (prim.attributes["$key:indices"] as? IntArray)?.let { index = it.getOrNull(index) ?: -1 }
        if (index < 0 || index >= values.size / 2) throw UsdFormatException("Invalid texture coordinates")
        val x = values[index * 2] * uvScale[0]; val y = values[index * 2 + 1] * uvScale[1]
        val angle = Math.toRadians(rotation.toDouble())
        return floatArrayOf((cos(angle) * x - sin(angle) * y).toFloat() + uvTranslation[0], (sin(angle) * x + cos(angle) * y).toFloat() + uvTranslation[1])
            .also { if (it.any { v -> !v.isFinite() }) throw UsdFormatException("Invalid texture coordinates") }
    }
    companion object {
        private fun floats(value: Any?): FloatArray? = when (value) { is FloatArray -> value; is DoubleArray -> FloatArray(value.size) { value[it].toFloat() }; else -> null }
        private fun linked(prim: UsdPrim, name: String, layer: UsdLayer): UsdPrim? = prim.relationships["$name.connect"]?.firstOrNull()?.substringBeforeLast('.')?.let { layer.prims[it] }
        fun resolve(owner: UsdPrim, mesh: UsdPrim, layer: UsdLayer, assets: UsdAssets): UsdMaterial {
            var ancestor: UsdPrim? = owner
            var material: UsdPrim? = null
            while (ancestor != null && material == null) {
                material = ancestor.relationships["material:binding"]?.firstOrNull()?.let { layer.prims[it] }
                ancestor = layer.prims[ancestor.path.substringBeforeLast('/', "")]
            }
            val surface = material?.let { linked(it, "outputs:surface", layer) }
                ?: material?.children?.mapNotNull { layer.prims[it] }?.firstOrNull { it.attributes["info:id"] == "UsdPreviewSurface" }
            val color = floats(surface?.attributes?.get("inputs:diffuseColor")) ?: floats(mesh.attributes["primvars:displayColor"]) ?: floatArrayOf(.78f, .78f, .76f)
            val shader = surface?.let { linked(it, "inputs:diffuseColor", layer) } ?: return UsdMaterial(color, null)
            if (shader.attributes["info:id"] != "UsdUVTexture") throw UsdFormatException("Unsupported diffuse material shader")
            val path = shader.attributes["inputs:file"] as? String ?: throw UsdFormatException("Missing texture asset")
            var reader = linked(shader, "inputs:st", layer)
            var scale = floatArrayOf(1f, 1f); var translation = FloatArray(2); var rotation = 0f
            if (reader?.attributes?.get("info:id") == "UsdTransform2d") {
                scale = floats(reader.attributes["inputs:scale"]) ?: scale
                translation = floats(reader.attributes["inputs:translation"]) ?: translation
                rotation = (reader.attributes["inputs:rotation"] as? Number)?.toFloat() ?: 0f
                reader = linked(reader, "inputs:in", layer)
            }
            val name = reader?.attributes?.get("inputs:varname") as? String ?: "st"
            val texture = SiteTexture(path, assets.image(path), shader.attributes["inputs:wrapS"] as? String ?: "repeat", shader.attributes["inputs:wrapT"] as? String ?: "repeat",
                floats(shader.attributes["inputs:scale"])?.takeIf { it.size == 4 } ?: floatArrayOf(1f, 1f, 1f, 1f), floats(shader.attributes["inputs:bias"])?.takeIf { it.size == 4 } ?: FloatArray(4))
            // A connection replaces the diffuse constant; UsdUVTexture owns its scale/bias.
            return UsdMaterial(floatArrayOf(1f, 1f, 1f), texture, name, scale, translation, rotation)
        }
    }
}
