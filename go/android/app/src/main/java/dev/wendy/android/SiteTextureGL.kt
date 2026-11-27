package dev.wendy.android

import android.graphics.BitmapFactory
import android.opengl.GLES20
import android.opengl.GLUtils

/** GL-thread ownership. Unit 8 is separate from the four live camera/depth pairs. */
internal class SiteTextureGL {
    private val uploaded = java.util.IdentityHashMap<ByteArray, Int>()
    private var pixels = 0
    private var unit = 0
    private var pixelsPerTexture = 8_388_608
    fun contextLost() { uploaded.clear(); pixels = 0; val count = IntArray(1); GLES20.glGetIntegerv(GLES20.GL_MAX_TEXTURE_IMAGE_UNITS, count, 0); unit = minOf(8, (count[0]-1).coerceAtLeast(0)) }
    fun clear(textureCount: Int = 1) {
        if (uploaded.isNotEmpty()) GLES20.glDeleteTextures(uploaded.size, uploaded.values.toIntArray(), 0)
        contextLost()
        pixelsPerTexture = 8_388_608 / textureCount.coerceAtLeast(1)
    }
    fun upload(texture: SiteTexture): Int = uploaded.getOrPut(texture.bytes) {
        val bounds = BitmapFactory.Options().apply { inJustDecodeBounds = true }
        BitmapFactory.decodeByteArray(texture.bytes, 0, texture.bytes.size, bounds)
        if (bounds.outWidth <= 0 || bounds.outHeight <= 0 || bounds.outWidth > 32768 || bounds.outHeight > 32768) return 0
        var sample = 1
        while (bounds.outWidth / sample > 1024 || bounds.outHeight / sample > 1024 || (bounds.outWidth.toLong() / sample) * (bounds.outHeight / sample) > pixelsPerTexture) {
            sample *= 2
            if (sample > 32768) return 0
        }
        val bitmap = BitmapFactory.decodeByteArray(texture.bytes, 0, texture.bytes.size, BitmapFactory.Options().apply { inSampleSize = sample; inScaled = false }) ?: return 0
        val ids = IntArray(1); GLES20.glGenTextures(1, ids, 0)
        GLES20.glActiveTexture(GLES20.GL_TEXTURE0 + unit)
        GLES20.glBindTexture(GLES20.GL_TEXTURE_2D, ids[0])
        GLES20.glTexParameteri(GLES20.GL_TEXTURE_2D, GLES20.GL_TEXTURE_MIN_FILTER, GLES20.GL_LINEAR)
        GLES20.glTexParameteri(GLES20.GL_TEXTURE_2D, GLES20.GL_TEXTURE_MAG_FILTER, GLES20.GL_LINEAR)
        GLUtils.texImage2D(GLES20.GL_TEXTURE_2D, 0, bitmap, 0)
        pixels += bitmap.width * bitmap.height; bitmap.recycle()
        ids[0]
    }
    fun bind(program: Int, id: Int, material: SiteTexture?) {
        GLES20.glActiveTexture(GLES20.GL_TEXTURE0 + unit)
        GLES20.glBindTexture(GLES20.GL_TEXTURE_2D, id)
        if (id != 0 && material != null) {
            fun wrap(value: String) = when (value) { "clamp", "black" -> GLES20.GL_CLAMP_TO_EDGE; "mirror" -> GLES20.GL_MIRRORED_REPEAT; else -> GLES20.GL_REPEAT }
            GLES20.glTexParameteri(GLES20.GL_TEXTURE_2D, GLES20.GL_TEXTURE_WRAP_S, wrap(material.wrapS))
            GLES20.glTexParameteri(GLES20.GL_TEXTURE_2D, GLES20.GL_TEXTURE_WRAP_T, wrap(material.wrapT))
        }
        GLES20.glUniform1i(GLES20.glGetUniformLocation(program, "uTexture"), unit)
        GLES20.glUniform1f(GLES20.glGetUniformLocation(program, "uTextured"), if (id == 0) 0f else 1f)
        GLES20.glUniform4fv(GLES20.glGetUniformLocation(program, "uTextureScale"), 1, material?.scale ?: floatArrayOf(1f,1f,1f,1f), 0)
        GLES20.glUniform4fv(GLES20.glGetUniformLocation(program, "uTextureBias"), 1, material?.bias ?: FloatArray(4), 0)
        GLES20.glActiveTexture(GLES20.GL_TEXTURE0)
    }
}
