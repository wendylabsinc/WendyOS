"""Install the managed VM's UDP DDS boundary before starting participants.

RTPS begins with its four-byte magic after the UDP header. Match the transport
header explicitly so IPv4 and IPv6 use the same rule, independently of ports.
Only the dedicated, owned table is replaced, in one nftables transaction.
Reference: https://netfilter.org/projects/nftables/manpage.html (raw payload).
"""

import json
import subprocess

TABLE = "wendy_g1"
OWNER = "Wendy managed G1 UDP DDS isolation v1"
RULESET = '''table inet wendy_g1 {
  comment "Wendy managed G1 UDP DDS isolation v1"
  chain input {
    type filter hook input priority -10; policy accept;
    meta l4proto != udp return
    iifname != "lo" @th,64,32 0x52545053 counter drop
  }
  chain output {
    type filter hook output priority -10; policy accept;
    meta l4proto != udp return
    oifname != "lo" @th,64,32 0x52545053 counter drop
  }
  chain forward {
    type filter hook forward priority -10; policy accept;
    meta l4proto != udp return
    @th,64,32 0x52545053 counter drop
  }
}
'''


def install():
    try:
        listed = subprocess.run(["nft", "--json", "list", "tables"],
                                check=True, capture_output=True, text=True, timeout=10)
    except subprocess.CalledProcessError:
        # Some WendyOS VM kernels provide legacy filtering without NF_TABLES.
        # Both families must independently verify before any DDS starts.
        from .isolation_legacy import install as install_legacy
        return install_legacy()
    tables = [entry["table"] for entry in json.loads(listed.stdout)["nftables"]
              if "table" in entry]
    existing = any(table["family"] == "inet" and table["name"] == TABLE for table in tables)
    prefix = ""
    if existing:
        # Ubuntu 22.04's nft 1.0.2 omits table comments from JSON output.
        listed = subprocess.run(["nft", "list", "table", "inet", TABLE],
                                check=True, capture_output=True, text=True, timeout=10)
        lines = [line.strip() for line in listed.stdout.splitlines()]
        if len(lines) < 2 or lines[0] != "table inet wendy_g1 {" or lines[1] != f'comment "{OWNER}"':
            raise RuntimeError("reserved nftables table has an unknown owner; refusing to replace it")
        prefix = "delete table inet wendy_g1\n"
    subprocess.run(["nft", "--file", "-"], input=prefix + RULESET,
                   check=True, capture_output=True, text=True, timeout=10)
    return {"dds_isolation": "udp-rtps-loopback", "table": "inet " + TABLE}


if __name__ == "__main__":
    try:
        print(json.dumps(install()), flush=True)
    except subprocess.CalledProcessError as error:
        detail = (error.stderr or error.stdout or "no diagnostic output").strip()
        raise SystemExit(f"Cannot enforce VM DDS isolation: {error.cmd!r} exited "
                         f"{error.returncode}: {detail}") from error
