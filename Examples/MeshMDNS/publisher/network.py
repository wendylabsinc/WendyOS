import os
import socket
import struct
import fcntl


def interface_ipv4():
    # Override only for local testing; deployed apps use their private eth0.
    if os.environ.get("MDNS_ADDRESS"):
        return os.environ["MDNS_ADDRESS"]
    interface = os.environ.get("MDNS_INTERFACE", "eth0")
    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as sock:
        packed = struct.pack("256s", interface.encode()[:15])
        result = fcntl.ioctl(sock.fileno(), 0x8915, packed)  # Linux SIOCGIFADDR
    return socket.inet_ntoa(result[20:24])
