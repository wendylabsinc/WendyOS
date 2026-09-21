"""Listener boundaries for local control and authenticated HIL inference."""
import ipaddress


def is_loopback(host):
    try:
        return ipaddress.ip_address(host).is_loopback
    except ValueError:
        return False


def require_loopback(host):
    # Use literal addresses so DNS cannot move a trusted listener off loopback.
    if not is_loopback(host):
        raise ValueError("Control services require a literal loopback address; use an authenticated Wendy tunnel")
