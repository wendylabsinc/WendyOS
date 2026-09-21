"""Legacy-iptables fallback for WendyOS kernels without CONFIG_NF_TABLES.

IPv4 and IPv6 INPUT/OUTPUT permit loopback; all other UDP packets containing
the four RTPS bytes anywhere are dropped, independent of domain or port.
FORWARD has the same UDP filter on every interface. This is broader than the
nft payload-prefix rule and may also reject unrelated UDP containing those
bytes. It does not claim isolation for TCP, opaque tunnels, or a privileged
application that changes the firewall. KMP handles non-linear packet buffers;
normal UDP RTPS headers are covered without assuming IPv4/IPv6 header offsets.

Only one reserved chain and its marked jumps in the filter table are changed.
Each family's --noflush restore is atomic; both are validated before commits,
and failure is propagated before the caller starts DDS. The caller must never
start participants after partial success. Existing protection is not removed
in a separate transaction, and no rollback creates a gap after an error.

References: netfilter iptables-extensions(8), string/KMP; iptables-restore(8),
--noflush, --test, --wait. See https://man7.org/linux/man-pages/man8/iptables-extensions.8.html
and https://man7.org/linux/man-pages/man8/iptables-restore.8.html.
"""

import shlex
import subprocess


CHAIN = "WENDY_G1_DDS"
OWNER = "wendy-g1-udp-isolation-v1"
FAMILIES = (("ipv4", "iptables-legacy", "iptables-legacy-restore"),
            ("ipv6", "ip6tables-legacy", "ip6tables-legacy-restore"))
DROP = ("-p", "udp", "-m", "string", "--hex-string", "|52545053|", "--algo", "kmp",
        "--to", "65535", "-m", "comment", "--comment", OWNER, "-j", "DROP")
JUMPS = {
    "INPUT": ("!", "-i", "lo", "-m", "comment", "--comment", OWNER, "-j", CHAIN),
    "OUTPUT": ("!", "-o", "lo", "-m", "comment", "--comment", OWNER, "-j", CHAIN),
    "FORWARD": ("-m", "comment", "--comment", OWNER, "-j", CHAIN),
}


def normalize(rule):
    """Normalize equivalent xt_string save formats, preserving other options.

    libxt_string prints this printable hex pattern as --string "RTPS". Its
    versions differ on whether the default 0/65535 search bounds are printed.
    """
    rule = list(rule)
    if "--string" in rule:
        index = rule.index("--string")
        if rule[index + 1:index + 2] == ["RTPS"]:
            rule[index:index + 2] = ["--hex-string", "|52545053|"]
    for option, default in (("--from", "0"), ("--to", "65535")):
        if option not in rule:
            continue
        index = rule.index(option)
        if rule[index + 1:index + 2] == [default]:
            del rule[index:index + 2]
    return tuple(rule)


def parse_listing(output):
    chains, rules = set(), {}
    for line in output.splitlines():
        fields = shlex.split(line)
        if not fields:
            continue
        if fields[0] in {"-P", "-N"} and len(fields) >= 2:
            chains.add(fields[1])
            rules.setdefault(fields[1], [])
        elif fields[0] == "-A" and len(fields) >= 3:
            rules.setdefault(fields[1], []).append(normalize(fields[2:]))
        else:
            raise RuntimeError("unexpected legacy filter-table listing; refusing to modify it")
    if not set(JUMPS) <= chains:
        raise RuntimeError("legacy filter table is missing INPUT/OUTPUT/FORWARD")
    return chains, rules


def owns(rule):
    return any(rule[index:index + 2] == ("--comment", OWNER) for index in range(len(rule) - 1))


def references(rule):
    return any(rule[index] in {"-j", "-g"} and rule[index + 1] == CHAIN
               for index in range(len(rule) - 1))


def plan(output):
    chains, rules = parse_listing(output)
    exists = CHAIN in chains
    if exists and (not rules.get(CHAIN) or not all(owns(rule) for rule in rules[CHAIN])):
        raise RuntimeError("reserved legacy DDS chain has an unknown owner; refusing to replace it")
    for source, entries in rules.items():
        for rule in entries:
            if references(rule) and (source not in JUMPS or rule != JUMPS[source]):
                raise RuntimeError("reserved legacy DDS chain has an unknown jump; refusing to modify it")
    # With --noflush, declaring this user chain creates it or clears only its
    # contents inside the pending transaction. Other chains are retained.
    lines = ["*filter", f":{CHAIN} - [0:0]",
             f"-A {CHAIN} " + " ".join(DROP)]
    for source, jump in JUMPS.items():
        # Reposition all of our known jumps atomically, before any unrelated
        # broad ACCEPT or ESTABLISHED rule. Unrelated rules retain their order.
        for rule in rules.get(source, []):
            if rule == jump:
                lines.append(f"-D {source} " + " ".join(jump))
        lines.append(f"-I {source} 1 " + " ".join(jump))
    lines.append("COMMIT")
    return "\n".join(lines) + "\n"


def verified(output):
    chains, rules = parse_listing(output)
    if CHAIN not in chains or rules.get(CHAIN) != [normalize(DROP)]:
        return False
    for source, jump in JUMPS.items():
        if not rules.get(source) or rules[source][0] != jump or rules[source].count(jump) != 1:
            return False
    return not any(references(rule) and (source not in JUMPS or rule != JUMPS[source])
                   for source, entries in rules.items() for rule in entries)


def install(*, run=subprocess.run):
    def listing(binary):
        return run([binary, "--wait", "5", "--table", "filter", "--list-rules"],
                   check=True, capture_output=True, text=True, timeout=10).stdout

    # Inspect ownership in both address families before changing either one.
    updates = [(family, binary, restore, plan(listing(binary)))
               for family, binary, restore in FAMILIES]
    for _, _, restore, update in updates:
        run([restore, "--wait", "5", "--noflush", "--test"], input=update,
            check=True, capture_output=True, text=True, timeout=10)
    for _, _, restore, update in updates:
        run([restore, "--wait", "5", "--noflush"], input=update,
            check=True, capture_output=True, text=True, timeout=10)
    for family, binary, _, _ in updates:
        if not verified(listing(binary)):
            raise RuntimeError(f"{family} legacy DDS isolation verification failed")
    return {"dds_isolation": "udp-rtps-loopback", "backend": "iptables-legacy",
            "families": ["ipv4", "ipv6"], "chain": CHAIN,
            "matcher": "UDP packet contains RTPS bytes (KMP); broader than a payload-prefix match"}
