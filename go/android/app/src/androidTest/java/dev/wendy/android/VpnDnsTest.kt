package dev.wendy.android

import android.util.Base64
import androidx.test.core.app.ApplicationProvider
import androidx.test.ext.junit.runners.AndroidJUnit4
import java.nio.ByteBuffer
import kotlinx.coroutines.*
import org.junit.Assert.*
import org.junit.Test
import org.junit.runner.RunWith

@RunWith(AndroidJUnit4::class)
class VpnDnsTest {
    @Test fun nativeEngineResolvesPrivateAndPublicNames(): Unit = runBlocking {
        val backend = ApplicationProvider.getApplicationContext<WendyApplication>().backend
        backend.initialize()
        try {
            backend.call("StartVPN", "", "")
            for (host in listOf("device-missing.mesh.wendy.dev", "example.com")) {
                val labels = host.split('.').flatMap { listOf(it.length.toByte()) + it.toByteArray().toList() }.toByteArray()
                val query = ByteBuffer.allocate(12 + labels.size + 5)
                    .putShort(0x1234).putShort(0x0100).putShort(1).putShort(0).putShort(0).putShort(0)
                    .put(labels).put(0).putShort(1).putShort(1).array()
                val packet = ByteBuffer.allocate(28 + query.size)
                packet.put(0x45).put(0).putShort(packet.capacity().toShort()).putInt(0)
                    .put(64).put(17).putShort(0).put(byteArrayOf(100, 64, 0, 54)).put(byteArrayOf(100, 64, 0, 53))
                    .putShort(45000.toShort()).putShort(53).putShort((8 + query.size).toShort()).putShort(0).put(query)
                var sum = (0 until 20 step 2).sumOf { packet.getShort(it).toInt() and 0xffff }
                while (sum > 0xffff) sum = (sum and 0xffff) + (sum ushr 16)
                packet.putShort(10, sum.inv().toShort())
                backend.call("VPNWrite", Base64.encodeToString(packet.array(), Base64.NO_WRAP))
                val reply = withTimeout(15000) {
                    var bytes: ByteArray? = null
                    while (bytes == null) {
                        val encoded = backend.call("VPNRead") as? String
                        if (!encoded.isNullOrEmpty()) bytes = Base64.decode(encoded, Base64.DEFAULT)
                    }
                    bytes
                }
                val offset = (reply[0].toInt() and 15) * 4 + 8
                val answer = ByteBuffer.wrap(reply, offset, reply.size - offset).slice()
                assertEquals("DNS transaction for $host", 0x1234, answer.getShort(0).toInt() and 0xffff)
                assertEquals("DNS response for $host", if (host == "example.com") 0 else 3, answer.getShort(2).toInt() and 15)
                if (host == "example.com") assertTrue("DNS answer for $host", answer.getShort(6) > 0)
            }
        } finally { backend.call("StopVPN") }
    }
}
