"""HTTP shutdown that drains active requests before resources are released."""
from http.server import ThreadingHTTPServer


class DrainingHTTPServer(ThreadingHTTPServer):
    daemon_threads = False
    closing = False

    def server_close(self):
        self.closing = True
        # ThreadingMixIn joins non-daemon handlers here. Handlers bound idle
        # reads and close persistent responses once draining has started.
        super().server_close()
