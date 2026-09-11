from std.python import Python


def main() raises:
    Python.add_to_path(".")
    var server = Python.import_module("http_bridge")
    server.serve("Hello from Mojo 1.0!", 8080)
