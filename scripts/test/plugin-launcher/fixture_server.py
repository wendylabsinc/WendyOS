#!/usr/bin/env python3
"""Serves launcher test fixtures from a directory on 127.0.0.1.

Usage: fixture_server.py ROOT PORT_FILE
Writes the chosen port to PORT_FILE, appends each GET path to ROOT/requests.log,
and sleeps FIXTURE_SLOW_SECONDS before answering when that variable is set.
"""
import http.server
import os
import sys
import time

root, port_file = sys.argv[1], sys.argv[2]
slow = float(os.environ.get("FIXTURE_SLOW_SECONDS", "0"))


class Handler(http.server.SimpleHTTPRequestHandler):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, directory=root, **kwargs)

    def do_GET(self):
        with open(os.path.join(root, "requests.log"), "a") as log:
            log.write(self.path + "\n")
        if slow:
            time.sleep(slow)
        super().do_GET()

    def log_message(self, *args):
        pass


server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
with open(port_file + ".tmp", "w") as f:
    f.write(str(server.server_address[1]))
os.replace(port_file + ".tmp", port_file)
server.serve_forever()
