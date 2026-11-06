package dev.wendy.android

import android.system.Os
import android.system.OsConstants
import androidx.test.ext.junit.runners.AndroidJUnit4
import java.io.FileDescriptor
import kotlinx.coroutines.*
import kotlinx.coroutines.channels.Channel
import org.junit.Assert.*
import org.junit.Test
import org.junit.runner.RunWith

@RunWith(AndroidJUnit4::class)
class VpnPacketPumpTest {
    @Test fun packetsFlowBothWaysAndIdlePumpCanStop() = runBlocking {
        val tunnel = FileDescriptor()
        val peer = FileDescriptor()
        Os.socketpair(OsConstants.AF_UNIX, OsConstants.SOCK_DGRAM, 0, tunnel, peer)
        Os.fcntlInt(tunnel, OsConstants.F_SETFL, OsConstants.O_NONBLOCK)
        val outgoing = Channel<ByteArray>(1)
        val incoming = Channel<ByteArray>(1)
        val pump = launch(Dispatchers.IO) {
            pumpVpnPackets(tunnel, receive = { incoming.receive() }, send = { outgoing.send(it) })
        }
        try {
            val request = byteArrayOf(0x45, 0, 1, 2, 3)
            Os.write(peer, request, 0, request.size)
            assertArrayEquals(request, withTimeout(2000) { outgoing.receive() })
            val response = byteArrayOf(0x45, 0, 4, 5, 6)
            incoming.send(response)
            val poll = android.system.StructPollfd().apply { fd = peer; events = OsConstants.POLLIN.toShort() }
            assertEquals(1, Os.poll(arrayOf(poll), 2000))
            val buffer = ByteArray(100)
            val size = Os.read(peer, buffer, 0, buffer.size)
            assertArrayEquals(response, buffer.copyOf(size))
            withTimeout(2000) { pump.cancelAndJoin() }
            assertTrue(tunnel.valid())
        } finally {
            pump.cancelAndJoin()
            Os.close(tunnel)
            Os.close(peer)
        }
    }
}
