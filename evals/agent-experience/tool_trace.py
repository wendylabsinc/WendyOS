"""Ordered tool traces from agent transcripts. Python standard library only."""

import json
import os
import shlex

CHAT_BUILTINS = frozenset({
    "workspace_list", "workspace_read", "workspace_write", "workspace_exec", "wendy_docs",
    "background_process_list", "background_process_stop", "memory_save", "memory_search",
    "memory_forget", "agent_delegate", "agent_remote", "camera_view", "audio_listen", "profile_skill"})
WRAPPERS = frozenset({"sudo", "env", "exec", "time", "nohup", "command"})
SHELLS = frozenset({"sh", "bash", "zsh", "fish"})
SPLITS = frozenset({"|", ";", "&&", "||", "&"})


def read_jsonl(path):
    records = []
    for line in path.read_text(errors="replace").splitlines():
        try:
            value = json.loads(line)
        except ValueError:
            continue
        if isinstance(value, dict):
            records.append(value)
    return records


VALUE_FLAGS = {"sudo": frozenset({"-u", "-g", "-C", "-D", "-h", "-p", "-U", "-r", "-t"}),
               "env": frozenset({"-u", "-C", "-S"})}


def split_lines(command):
    """Split on newlines outside quotes; a backslash-newline continues the line."""
    lines, current, quote, i = [], [], None, 0
    while i < len(command):
        ch = command[i]
        if ch == "\\" and quote != "'" and command[i + 1:i + 2] == "\n":
            i += 2
            continue
        if ch == "\\" and quote != "'":
            current.append(command[i:i + 2])
            i += 2
            continue
        if quote:
            quote = None if ch == quote else quote
        elif ch in "'\"":
            quote = ch
        elif ch == "\n":
            lines.append("".join(current))
            current = []
            i += 1
            continue
        current.append(ch)
        i += 1
    lines.append("".join(current))
    return lines


def drop_redirections(words):
    kept, i = [], 0
    while i < len(words):
        word = words[i]
        if set(word) <= set("<>&") and set(word) & set("<>"):
            if kept and kept[-1] in ("0", "1", "2"):
                kept.pop()
            i += 2
        elif set(word) <= set("()"):
            i += 1
        else:
            kept.append(word)
            i += 1
    return kept


def parse_wendy_invocations(command):
    found = []
    for line in split_lines(command):
        found.extend(parse_line(line))
    return found


def parse_line(command):
    try:
        lexer = shlex.shlex(command, posix=True, punctuation_chars=True)
        lexer.whitespace_split = True
        tokens = list(lexer)
    except ValueError:
        return []
    segments, current = [], []
    for token in tokens:
        if token in SPLITS:
            segments.append(current)
            current = []
        else:
            current.append(token)
    segments.append(current)
    found = []
    for words in segments:
        words = drop_redirections(words)
        i, wrapper = 0, None
        while i < len(words):
            word = words[i]
            if word in WRAPPERS:
                wrapper = word
            elif wrapper and word.startswith("-"):
                i += word in VALUE_FLAGS.get(wrapper, ())
            elif "=" not in word or word.startswith("="):
                break
            i += 1
        if i >= len(words):
            continue
        program, rest = os.path.basename(words[i]), words[i + 1:]
        if program == "wendy":
            found.append(rest)
        elif program in SHELLS:
            for j, word in enumerate(rest[:-1]):
                if word in ("-c", "-lc"):
                    found.extend(parse_wendy_invocations(rest[j + 1]))
                    break
    return found


def cli_path(argv):
    words, i = [], 0
    while i < len(argv):
        word = argv[i]
        if word == "--json" or word.startswith("--device="):
            i += 1
        elif word == "--device":
            i += 2
        elif word.startswith("-"):
            break
        else:
            words.append(word)
            i += 1
    return words


def command_words(argv):
    """The words of a wendy argv with every flag token skipped (and --device's value), up to "--".

    Looser than cli_path, which stops at the first flag: `vm --yes stop X` gives vm stop X. A flag's
    value still counts as a word (`--profile go2` adds go2). For capture lookups and canned actions;
    the allowlist keeps the strict cli_path.
    """
    words, i = [], 0
    while i < len(argv):
        word = argv[i]
        if word == "--":
            break
        if word == "--device":
            i += 2
            continue
        if not word.startswith("-"):
            words.append(word)
        i += 1
    return words


def entry(channel, name, arguments, source="transcript", canned=False, captured=None, is_error=None,
          reply_text=None):
    return {"channel": channel, "name": name, "arguments": arguments, "source": source,
            "canned": canned, "captured": captured, "is_error": is_error, "reply_text": reply_text}


def shell_entries(command):
    command = command if isinstance(command, str) else ""
    return [entry("shell", "shell", command)] + [
        entry("cli", " ".join(cli_path(argv)), argv) for argv in parse_wendy_invocations(command)]


def _reply(value):
    if isinstance(value, str):
        return value
    return json.dumps(value, ensure_ascii=False) if value is not None else None


def _error(value):
    """Only explicit protocol errors; ordinary prose containing 'error' is not a failure."""
    if isinstance(value, str):
        try:
            value = json.loads(value)
        except ValueError:
            return value.startswith(("Error:", "error:", "Tool failed:", "Denied:"))
    return isinstance(value, dict) and (value.get("isError") is True or bool(value.get("error")))


def _annotate(entries, result, failed=None, call_id=None, agent_id=""):
    for item in entries:
        if result is not None:
            item["reply_text"] = _reply(result)
            item["is_error"] = _error(result) if failed is None else failed
        elif failed is not None:
            item["is_error"] = failed
        item["call_id"], item["agent_id"] = call_id, agent_id
    return entries


def claude_entries(events):
    results = {}
    for event in events:
        for block in event.get("message", {}).get("content", []) if isinstance(event.get("message", {}).get("content"), list) else []:
            if block.get("type") == "tool_result":
                results[block.get("tool_use_id")] = block
    for event in events:
        if event.get("type") != "assistant":
            continue
        for block in event.get("message", {}).get("content", []):
            if block.get("type") != "tool_use":
                continue
            name, args = block.get("name", ""), block.get("input", {})
            if name.startswith("mcp__wendy__"):
                items = [entry("mcp", name[len("mcp__wendy__"):], args)]
            elif name == "Bash":
                items = shell_entries(args.get("command"))
            else:
                items = [entry("builtin", name, args)]
            reply = results.get(block.get("id"))
            yield _annotate(items, reply.get("content") if reply else None,
                            bool(reply.get("is_error")) if reply else None, block.get("id"))


def codex_entries(events):
    items = {}
    for index, event in enumerate(events):
        item = event.get("item")
        if event.get("type") not in ("item.started", "item.completed") or not isinstance(item, dict):
            continue
        key = item.get("id", f"#{index}")
        if event["type"] == "item.completed" or key not in items:
            items[key] = item
    for item in items.values():
        kind = item.get("type")
        if kind == "mcp_tool_call":
            group = [entry("mcp" if item.get("server") == "wendy" else "builtin",
                           item.get("tool", "") if item.get("server") == "wendy" else
                           f"{item.get('server')}.{item.get('tool')}", item.get("arguments", {}))]
            reply = item.get("result")
            failed = True if item.get("error") or item.get("status") == "failed" else _error(reply) if reply is not None else None
        elif kind == "command_execution":
            group = shell_entries(item.get("command"))
            reply = item.get("aggregated_output")
            failed = item["exit_code"] != 0 if isinstance(item.get("exit_code"), int) else None
        elif kind in ("agent_message", "reasoning"):
            continue
        else:
            group, reply, failed = [entry("builtin", str(kind), {})], None, None
        yield _annotate(group, reply, failed, item.get("id"))


def wendy_entries(events):
    seen = set()
    results = {(e.get("agent_id", ""), e.get("id")): e for e in events if e.get("type") == "tool_result"}
    for index, event in enumerate(events):
        if event.get("type") != "tool_start":
            continue
        key = (event.get("agent_id", ""), event.get("id", f"#{index}"))
        if key in seen:
            continue
        seen.add(key)
        name, args = event.get("tool", ""), event.get("arguments", {})
        if name == "workspace_exec":
            group = shell_entries(args.get("command") if isinstance(args, dict) else None)
        elif name in CHAT_BUILTINS:
            group = [entry("builtin", name, args)]
        else:
            group = [entry("mcp", name, args)]
        reply = results.get(key)
        yield _annotate(group, reply.get("text") if reply else None, call_id=key[1], agent_id=key[0])


def build_trace(adapter, events, proxy_records=None, cli_records=None):
    sources = {"claude": claude_entries, "codex": codex_entries, "wendy": wendy_entries}
    if adapter not in sources:
        raise ValueError(f"unknown adapter: {adapter}")
    trace = [e for group in sources[adapter](events) for e in group]
    recorded = []
    if proxy_records is not None:
        trace = [e for e in trace if e["channel"] != "mcp"]
        recorded += [(r.get("t"), entry("mcp", r.get("tool", ""), r.get("arguments", {}), "proxy",
                        bool(r.get("canned")), r.get("captured"), r.get("is_error"), r.get("reply_text")))
                  for r in proxy_records]
    if cli_records is not None:
        trace = [e for e in trace if e["channel"] != "cli"]
        recorded += [(r.get("t"), entry("cli", " ".join(cli_path(r.get("argv", []))), r.get("argv", []), "wrapper",
                        bool(r.get("canned")), r.get("captured"),
                        r["exit_code"] != 0 if isinstance(r.get("exit_code"), int) else None,
                        r.get("reply_text"))) for r in cli_records]
    if proxy_records:
        for (_, item), record in zip(recorded[:len(proxy_records)], proxy_records):
            item['reply_bytes'] = record.get('reply_bytes')
    # Proxy and wrapper clocks use the same host time, so mixed MCP/CLI sequences retain order.
    if recorded and all(isinstance(t, (float, int)) for t, _ in recorded):
        recorded.sort(key=lambda item: item[0])
    elif proxy_records and cli_records:
        for _, item in recorded:
            item["order_unknown"] = True
    return trace + [item for _, item in recorded]
