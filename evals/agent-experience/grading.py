"""Grade the tool choice in a trace against a task's `expect` block."""
import json
import re

from tool_trace import cli_path, command_words, parse_wendy_invocations

OUTCOMES = ("right", "wrong", "missed", "asked", "went_around")
EXPECT_KEYS = {"right", "wrong", "ask"}
MATCHER_KEYS = {"tool", "cli", "shell", "args", "args_regex", "agents", "block"}
KINDS = ("tool", "cli", "shell")
HELP_FLAGS = frozenset({"--help", "-h"})


def validate_expect(expect):
    if not isinstance(expect, dict) or set(expect) - EXPECT_KEYS:
        raise ValueError(f"expect keys must be within {sorted(EXPECT_KEYS)}")
    if not isinstance(expect.get("ask", False), bool):
        raise ValueError("expect.ask must be a boolean")
    for side in ("right", "wrong"):
        matchers = expect.get(side, [])
        if not isinstance(matchers, list):
            raise ValueError(f"expect.{side} must be a list")
        for matcher in matchers:
            if not isinstance(matcher, dict) or set(matcher) - MATCHER_KEYS:
                raise ValueError(f"unknown matcher keys in {matcher!r}")
            if sum(k in matcher for k in KINDS) != 1:
                raise ValueError(f"matcher needs exactly one of tool, cli, shell: {matcher!r}")
            kind = next(k for k in KINDS if k in matcher)
            if not isinstance(matcher[kind], str):
                raise ValueError(f"{kind} must be a string: {matcher!r}")
            if "args" in matcher and (kind != "tool" or not isinstance(matcher["args"], dict)):
                raise ValueError(f"args must be an object and is only for tool matchers: {matcher!r}")
            if "args_regex" in matcher and (kind == "shell" or not isinstance(matcher["args_regex"], str)):
                raise ValueError(f"args_regex must be a string and is not for shell matchers: {matcher!r}")
            if "agents" in matcher and not (isinstance(matcher["agents"], list)
                                            and all(isinstance(a, str) for a in matcher["agents"])):
                raise ValueError(f"agents must be a list of strings: {matcher!r}")
            if "block" in matcher and not isinstance(matcher["block"], bool):
                raise ValueError(f"block must be a boolean: {matcher!r}")
            for key in ("shell", "args_regex"):
                if key in matcher:
                    try:
                        re.compile(matcher[key])
                    except re.error as error:
                        raise ValueError(f"invalid regex in {key}: {error}") from error


def is_help(argv):
    """True when a wendy argv asks for help: --help or -h before any "--" (later words are the remote command's)."""
    for word in argv if isinstance(argv, list) else []:
        if word == "--":
            return False
        if word in HELP_FLAGS:
            return True
    return False


def _arg_ok(actual, want):
    return want in actual if isinstance(actual, list) else actual == want


def matches(matcher, entry, adapter=None):
    if "agents" in matcher and adapter not in matcher["agents"]:
        return False
    channel, name, arguments = entry.get("channel"), entry.get("name") or "", entry.get("arguments")
    if "tool" in matcher:
        if channel != "mcp" or name != matcher["tool"]:
            return False
        arguments = arguments if isinstance(arguments, dict) else {}
        if any(k not in arguments or not _arg_ok(arguments[k], v) for k, v in matcher.get("args", {}).items()):
            return False
        text = json.dumps(arguments, sort_keys=True)
    elif "cli" in matcher:
        # A help lookup is informational, so it is neither a right nor a wrong path.
        if channel != "cli" or is_help(arguments):
            return False
        # The words prefix-match the strict path (stops at the first flag) or the flag-skipping words the
        # shim cans and looks captures up by, so `vm --yes create` grades like the `vm create` it canned.
        want = matcher["cli"].split()
        paths = [cli_path(arguments), command_words(arguments)] if isinstance(arguments, list) else [name.split()]
        if not any(words[:len(want)] == want for words in paths):
            return False
        text = " ".join(arguments) if isinstance(arguments, list) else ""
    else:
        return channel == "shell" and re.search(matcher["shell"], arguments if isinstance(arguments, str) else "") is not None
    return "args_regex" not in matcher or re.search(matcher["args_regex"], text) is not None


def _first(matchers, trace, adapter):
    for i, matcher in enumerate(matchers):
        for j, entry in enumerate(trace):
            if matches(matcher, entry, adapter):
                return i, j
    return None


def _plain_shell(entry):
    """A shell command that runs no wendy at all: the model answered with Linux commands."""
    command = entry.get("arguments")
    return (entry.get("channel") == "shell" and isinstance(command, str)
            and not parse_wendy_invocations(command))


def grade(trace, expect, final_text, agent_completed, group=None, adapter=None, went_around=False):
    """Order: wrong, right, went around, asked, missed. `went_around` is set only where the model shares
    the device (the local target), so a laptop's grades don't change."""
    result = {"outcome": "missed", "rule": None, "entry": None, "hidden_group": False}
    for side in ("wrong", "right"):
        hit = _first(expect.get(side, []), trace, adapter)
        if hit:
            result.update(outcome=side, rule=f"{side}[{hit[0]}]", entry=hit[1])
            return result
    plain = next((i for i, e in enumerate(trace) if _plain_shell(e)), None) if went_around else None
    if plain is not None:
        result.update(outcome="went_around", entry=plain)
        return result
    if expect.get("ask") and agent_completed and "?" in (final_text or ""):
        result["outcome"] = "asked"
        return result
    if adapter in ("claude", "codex") and group and group != "core":
        enabled = any(e.get("channel") == "mcp" and e.get("name") == "wendy_tools"
                      and isinstance(e.get("arguments"), dict)
                      and {group, "all"} & set(e["arguments"].get("groups") or [])
                      for e in trace)
        result["hidden_group"] = not enabled
    return result
