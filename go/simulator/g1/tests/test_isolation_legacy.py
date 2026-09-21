import subprocess
from types import SimpleNamespace

import pytest

from g1_sim.isolation_legacy import CHAIN, DROP, FAMILIES, JUMPS, OWNER, install, plan, verified


EMPTY = "-P INPUT ACCEPT\n-P FORWARD DROP\n-P OUTPUT ACCEPT\n"


def installed(*, late=False, duplicates=False):
    lines = [EMPTY.rstrip(), f"-N {CHAIN}", f"-A {CHAIN} " + " ".join(DROP)]
    for chain, rule in JUMPS.items():
        unrelated = f"-A {chain} -p tcp -j ACCEPT"
        if late:
            lines.append(unrelated)
        lines.append(f"-A {chain} " + " ".join(rule))
        if duplicates:
            lines.append(f"-A {chain} " + " ".join(rule))
        if not late:
            lines.append(unrelated)
    return "\n".join(lines) + "\n"


def test_new_rules_cover_both_directions_and_forward_without_changing_other_policy():
    patch = plan(EMPTY + "-A INPUT -p tcp -j ACCEPT\n")
    assert patch.startswith(f"*filter\n:{CHAIN} - [0:0]\n")
    assert patch.endswith("COMMIT\n")
    assert "--algo kmp" in patch and "--hex-string |52545053|" in patch
    assert "-I INPUT 1 ! -i lo" in patch and "-I OUTPUT 1 ! -o lo" in patch
    assert "-I FORWARD 1" in patch
    assert "--dport" not in patch and "--sport" not in patch
    assert "-P " not in patch and "*nat" not in patch and "-F INPUT" not in patch


def test_reinstall_repositions_owned_jumps_atomically_and_preserves_unrelated_rules():
    prior = installed(late=True, duplicates=True)
    patch = plan(prior)
    assert f":{CHAIN} - [0:0]\n" in patch and "-F " not in patch
    assert patch.count("-D INPUT ") == 2 and patch.count("-I INPUT 1 ") == 1
    assert "-p tcp" not in patch
    assert not verified(prior)
    assert verified(installed())
    assert verified(installed().replace("--to 65535", "--from 0 --to 65535"))


@pytest.mark.parametrize("bounds", ["--to 65535", "", "--from 0 --to 65535"])
def test_actual_xt_string_saved_ascii_and_default_bounds_are_verified(bounds):
    # xt_string does not retain whether printable bytes were supplied as hex.
    listing = installed().replace("--hex-string |52545053|", '--string "RTPS"')
    listing = listing.replace("--to 65535", bounds)
    assert verified(listing)
    assert "-I INPUT 1" in plan(listing)


@pytest.mark.parametrize("change", ["--string rtps", "--string RTP", "--string RPTS"])
def test_different_payload_string_is_never_verified(change):
    assert not verified(installed().replace("--hex-string |52545053|", change))


def test_limited_string_search_range_is_never_verified():
    assert not verified(installed().replace("--to 65535", "--to 60"))
    assert not verified(installed().replace("--to 65535", "--from 40 --to 65535"))


@pytest.mark.parametrize("foreign", [
    EMPTY + f"-N {CHAIN}\n",
    EMPTY + f"-N {CHAIN}\n-A {CHAIN} -j ACCEPT\n",
    installed() + f"-A {CHAIN} -p tcp -j RETURN\n",
    installed() + f"-A INPUT -j {CHAIN}\n",
    installed() + f"-N SOME_OTHER_CHAIN\n-A SOME_OTHER_CHAIN -m comment --comment {OWNER} -j {CHAIN}\n",
])
def test_foreign_reserved_chains_or_jumps_are_refused(foreign):
    with pytest.raises(RuntimeError, match="unknown"):
        plan(foreign)


class Commands:
    def __init__(self, *, conflict=False, fail_restore=None, broken_verify=False):
        self.calls = []
        self.conflict, self.fail_restore, self.broken_verify = conflict, fail_restore, broken_verify
        self.committed = set()

    def __call__(self, command, **kwargs):
        self.calls.append((command, kwargs))
        assert kwargs["check"] and kwargs["timeout"] == 10
        if "--list-rules" in command:
            family = "ipv6" if command[0].startswith("ip6") else "ipv4"
            if self.conflict and family == "ipv6":
                return SimpleNamespace(stdout=EMPTY + f"-N {CHAIN}\n")
            return SimpleNamespace(stdout=installed() if family in self.committed and not self.broken_verify else EMPTY)
        assert "--noflush" in command and "--wait" in command
        family = "ipv6" if command[0].startswith("ip6") else "ipv4"
        if "--test" not in command:
            if self.fail_restore == family:
                raise subprocess.CalledProcessError(1, command, stderr="kernel rejected transaction")
            self.committed.add(family)
        return SimpleNamespace(stdout="")


def test_both_families_preflight_then_commit_then_verify_before_success():
    commands = Commands()
    result = install(run=commands)
    assert result["families"] == ["ipv4", "ipv6"]
    assert len(commands.calls) == 8
    assert all("--list-rules" in command for command, _ in commands.calls[:2])
    assert all("--test" in command for command, _ in commands.calls[2:4])
    assert all("--test" not in command for command, _ in commands.calls[4:6])
    assert commands.committed == {"ipv4", "ipv6"}


def test_unknown_ipv6_owner_prevents_any_ipv4_mutation():
    commands = Commands(conflict=True)
    with pytest.raises(RuntimeError, match="unknown"):
        install(run=commands)
    assert len(commands.calls) == 2 and not commands.committed


def test_ipv6_failure_leaves_ipv4_protection_and_never_claims_success_or_rolls_back():
    commands = Commands(fail_restore="ipv6")
    with pytest.raises(subprocess.CalledProcessError):
        install(run=commands)
    assert commands.committed == {"ipv4"}
    assert len(commands.calls) == 6


def test_failed_readback_is_not_treated_as_installed():
    with pytest.raises(RuntimeError, match="verification failed"):
        install(run=Commands(broken_verify=True))
