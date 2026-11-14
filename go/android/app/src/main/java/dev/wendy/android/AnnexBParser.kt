package dev.wendy.android

/** Streaming Annex-B parser; transport messages need not align with NAL units. */
class AnnexBParser {
    private var buffer = ByteArray(0)
    fun append(chunk: ByteArray): List<ByteArray> {
        if (chunk.size > LIMIT) { buffer = ByteArray(0); return emptyList() }
        if (buffer.size + chunk.size > LIMIT) buffer = ByteArray(0)
        buffer += chunk
        val result = mutableListOf<ByteArray>()
        while (true) {
            val first = start(0)
            if (first == null) { buffer = buffer.takeLast(3).toByteArray(); return result }
            if (first.first > 0) buffer = buffer.copyOfRange(first.first, buffer.size)
            val next = start(first.second) ?: return result
            if (next.first > first.second) result.add(buffer.copyOfRange(first.second, next.first))
            buffer = buffer.copyOfRange(next.first, buffer.size)
        }
    }
    private fun start(offset: Int): Pair<Int, Int>? {
        for (index in offset until buffer.size - 2) {
            if (buffer[index] == 0.toByte() && buffer[index + 1] == 0.toByte()) {
                if (buffer[index + 2] == 1.toByte()) return index to 3
                if (index + 3 < buffer.size && buffer[index + 2] == 0.toByte() && buffer[index + 3] == 1.toByte()) return index to 4
            }
        }
        return null
    }
    companion object { private const val LIMIT = 16 * 1024 * 1024 }
}
