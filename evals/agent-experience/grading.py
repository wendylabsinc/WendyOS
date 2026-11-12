"""Grade the tool choice in a trace against a task's `expect` block."""
import json
import re

from tool_trace import cli_path, command_words, parse_wendy_invocations

OUTCOMES = ("right", "wrong", "missed", "asked", "went_around")
EXPECT_KEYS = {"right", "wrong", "ask", "ask_regex", "sequence", "require_result"}
MATCHER_KEYS = {"tool", "cli", "shell", "args", "args_regex", "agents", "block"}
KINDS = ("tool", "cli", "shell")
HELP_FLAGS = frozenset({"--help", "-h"})


def validate_expect(expect):
    if not isinstance(expect, dict) or set(expect) - EXPECT_KEYS:
        raise ValueError(f"expect keys must be within {sorted(EXPECT_KEYS)}")
    if not isinstance(expect.get("ask", False), bool):
        raise ValueError("expect.ask must be a boolean")
    if not isinstance(expect.get("require_result", False), bool):
        raise ValueError("expect.require_result must be a boolean")
    patterns = expect.get("ask_regex", [])
    if not isinstance(patterns, list) or any(not isinstance(p, str) or not p for p in patterns):
        raise ValueError("expect.ask_regex must be a list of nonempty regular expressions")
    if expect.get("ask") and not patterns:
        raise ValueError("expect.ask requires scenario-specific ask_regex criteria")
    for pattern in patterns:
        try:
            re.compile(pattern)
        except re.error as error:
            raise ValueError(f"invalid ask_regex: {error}") from error
    sequence = expect.get("sequence", [])
    if not isinstance(sequence, list):
        raise ValueError("expect.sequence must be a list of alternative-matcher lists")
    for step in sequence:
        if not isinstance(step, list) or not step:
            raise ValueError("each sequence step needs at least one matcher")
        validate_expect({"right": step})
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
    if isinstance(actual, list) and not isinstance(want, list):
        return any(_arg_ok(item, want) for item in actual)
    if isinstance(actual, bool) != isinstance(want, bool):
        return False
    return actual == want


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


def _placeholder(entry):
    """A proxy fallback reply: a canned MCP tool whose capture is missing. A CLI call the shim cans without
    a capture is its designed silent success; commands whose output matters are listed in cli_canned, and
    readiness requires those captures."""
    return entry.get("channel") == "mcp" and entry.get("canned") and entry.get("captured") is False


def _first(matchers, trace, adapter, require_result=False, right=False):
    for j, entry in enumerate(trace):
        for i, matcher in enumerate(matchers):
            if right and (entry.get("is_error") is True or
                          (require_result and _placeholder(entry)) or
                          (require_result and entry.get("is_error") is not False)):
                continue
            if matches(matcher, entry, adapter):
                return i, j
    return None


def _plain_shell(entry):
    """A shell command that runs no wendy at all: the model answered with Linux commands."""
    command = entry.get("arguments")
    return (entry.get("channel") == "shell" and isinstance(command, str)
            and not parse_wendy_invocations(command))


SESSION_TOOLS = {"wendy_status", "wendy_tools", "device_list", "device_connect", "device_disconnect"}
CLI_NO_WORK = {"-h", "--help", "-v", "--version"}


def _wendy_work(entry):
    """A Wendy call that does something: not connecting, listing tools or reading help."""
    if entry.get("channel") == "mcp":
        return entry.get("name") not in SESSION_TOOLS
    if entry.get("channel") == "cli":
        argv = entry.get("arguments") if isinstance(entry.get("arguments"), list) else []
        argv = [word for word in argv if isinstance(word, str)]
        path = cli_path(argv)
        return bool(path) and path != ["version"] and not CLI_NO_WORK & set(argv)
    return False


def explicit_target(entry):
    args = entry.get("arguments")
    if entry.get("channel") == "mcp" and isinstance(args, dict):
        return args.get("device") or (args.get("selector") if entry.get("name") == "device_connect" else None)
    if entry.get("channel") == "cli" and isinstance(args, list):
        for i, value in enumerate(args):
            if value == "--":
                break
            if value == "--device" and i + 1 < len(args):
                return args[i + 1]
            if isinstance(value, str) and value.startswith("--device="):
                return value.split("=", 1)[1]
    return None


def grade(trace, expect, final_text, agent_completed, group=None, adapter=None, went_around=False, target=None):
    """Order: wrong, right, went around, asked, missed. `went_around` is set only where the model shares
    the device (the local target), so a laptop's grades don't change."""
    result = {"outcome": "missed", "rule": None, "entry": None, "hidden_group": False}
    if target:
        for i, entry in enumerate(trace):
            selected = explicit_target(entry)
            if selected is not None and selected != target:
                result.update(outcome="wrong", rule="target", entry=i)
                return result
    for side in ("wrong", "right"):
        hit = _first(expect.get(side, []), trace, adapter, expect.get("require_result", False), right=side == "right")
        if side == "right" and hit and expect.get("sequence"):
            cursor = 0
            for alternatives in expect["sequence"]:
                step = _first(alternatives, trace[cursor:], adapter, expect.get("require_result", False), right=True)
                if step is None or trace[cursor + step[1]].get("order_unknown"):
                    hit = None
                    result["rule"] = "incomplete_sequence"
                    break
                cursor += step[1] + 1
        if hit:
            result.update(outcome=side, rule=f"{side}[{hit[0]}]", entry=hit[1])
            return result
    if went_around:  # plain Linux instead of Wendy: at least as many plain commands as Wendy calls
        plain = [i for i, e in enumerate(trace) if _plain_shell(e)]
        if plain and len(plain) >= sum(_wendy_work(e) for e in trace):
            result.update(outcome="went_around", entry=plain[0])
            return result
    # Every required concept must occur in a question, not in an unrelated success claim.
    questions = re.findall(r"[^.!?\n]*\?", final_text or "")
    if (expect.get("ask") and agent_completed and expect.get("ask_regex") and
            any(all(re.search(p, question, re.I) for p in expect["ask_regex"]) for question in questions)):
        result["outcome"] = "asked"
        return result
    if adapter in ("claude", "codex") and group and group != "core":
        enabled = any(e.get("channel") == "mcp" and e.get("name") == "wendy_tools"
                      and isinstance(e.get("arguments"), dict)
                      and {group, "all"} & set(e["arguments"].get("groups") or [])
                      for e in trace)
        result["hidden_group"] = not enabled
    return result
