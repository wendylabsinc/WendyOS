package dev.wendy.android

import android.system.ErrnoException
import android.system.Os
import android.system.OsConstants
import android.system.StructPollfd
import java.io.FileDescriptor
import java.io.IOException
import kotlinx.coroutines.*

/** Nonblocking TUN I/O with bounded waits so cancellation also works when idle. */
internal suspend fun pumpVpnPackets(
    descriptor: FileDescriptor,
    receive: suspend () -> ByteArray?,
    send: suspend (ByteArray) -> Unit,
) = coroutineScope {
    suspend fun ready(events: Int) {
        val poll = StructPollfd().apply { fd = descriptor; this.events = events.toShort() }
        while (true) {
            ensureActive()
            try {
                if (Os.poll(arrayOf(poll), 250) == 0) continue
            } catch (e: ErrnoException) {
                if (e.errno == OsConstants.EINTR) continue
                throw e
            }
            ensureActive()
            if (poll.revents.toInt() and (OsConstants.POLLERR or OsConstants.POLLHUP or OsConstants.POLLNVAL) != 0) {
                throw IOException("VPN interface closed")
            }
            if (poll.revents.toInt() and events != 0) return
        }
    }
    launch(Dispatchers.IO) {
        val buffer = ByteArray(65535)
        while (isActive) {
            ready(OsConstants.POLLIN)
            val size = try { Os.read(descriptor, buffer, 0, buffer.size) }
            catch (e: ErrnoException) { if (e.errno == OsConstants.EAGAIN || e.errno == OsConstants.EINTR) continue else throw e }
            if (size <= 0) throw IOException("VPN interface closed")
            send(buffer.copyOf(size))
        }
    }
    launch(Dispatchers.IO) {
        while (isActive) {
            val packet = receive()
            if (packet == null || packet.isEmpty()) continue
            while (true) {
                ready(OsConstants.POLLOUT)
                val written = try { Os.write(descriptor, packet, 0, packet.size) }
                catch (e: ErrnoException) { if (e.errno == OsConstants.EAGAIN || e.errno == OsConstants.EINTR) continue else throw e }
                if (written != packet.size) throw IOException("Incomplete VPN packet")
                break
            }
        }
    }
}
