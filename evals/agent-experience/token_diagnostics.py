"""Provider accounting and separately labelled tool payload estimates. No token attribution claims."""
import collections
import json
import math
import statistics


def tool_payloads(trace):
    totals = {}
    seen = collections.Counter()
    previous_errors = set()
    for item in trace:
        # A shell invocation and its extracted CLI calls share the same reply. Count it once.
        if item.get('channel') == 'cli' and item.get('source') == 'transcript':
            continue
        name = item.get('name') or 'unknown'
        key = (item.get('agent_id', ''), item.get('channel'), name,
               json.dumps(item.get('arguments'), sort_keys=True))
        value = totals.setdefault(name, {'calls': 0, 'repeated_calls': 0, 'retries_after_error': 0,
                                        'result_bytes': 0, 'unknown_results': 0})
        value['calls'] += 1
        value['repeated_calls'] += seen[key] > 0
        value['retries_after_error'] += key in previous_errors
        seen[key] += 1
        if item.get('is_error'):
            previous_errors.add(key)
        else:
            previous_errors.discard(key)
        reply = item.get('reply_text')
        if isinstance(item.get('reply_bytes'), int):
            value['result_bytes'] += item['reply_bytes']
        elif reply is None:
            value['unknown_results'] += 1
        else:
            value['result_bytes'] += len(reply.encode('utf-8'))
    for value in totals.values():
        value['estimated_result_tokens'] = math.ceil(value['result_bytes'] / 4)
    return totals


def diagnostics(adapter, events, trace, normalize):
    records = []
    for index, event in enumerate(events):
        kind = event.get('type')
        if (adapter, kind) not in {('wendy', 'usage'), ('codex', 'turn.completed'), ('claude', 'result')}:
            continue
        raw = event.get('usage') or {}
        usage = normalize(raw, anthropic=adapter == 'claude')
        records.append({'event_index': index, 'agent_id': event.get('agent_id', ''),
                        'model': raw.get('model'), 'tokens': usage,
                        'complete': usage is not None and (adapter != 'wendy' or raw.get('complete') is True),
                        'scope': {'wendy': 'request', 'codex': 'turn', 'claude': 'session'}[adapter]})
    if adapter == 'claude':
        records = records[-1:]  # session snapshots are cumulative
    return {'provider_usage': records, 'tools': tool_payloads(trace),
            'estimate_method': 'UTF-8 result bytes / 4, rounded up; not tokenizer counts or billed tokens. '
                               'Repeated context, images, tool schemas and caching are not attributed to tools.'}


def render(rows):
    lines = ['# Token diagnostics', '',
             'Provider counts retain their request, turn or session scope. Tool estimates use UTF-8 bytes / 4; '
             'they are not billed usage. Repeated calls may be intentional polling.', '',
             '| Attempt | Tool | Calls | Repeated | After error | Result bytes | Estimated tokens | Unknown results |',
             '| --- | --- | --- | --- | --- | --- | --- | --- |']
    for row in rows:
        for name, value in row.get('metrics', {}).get('token_diagnostics', {}).get('tools', {}).items():
            cells = [row.get('id', row.get('task', '?')), name, value['calls'], value['repeated_calls'],
                     value['retries_after_error'], value['result_bytes'], value['estimated_result_tokens'], value['unknown_results']]
            lines.append('| ' + ' | '.join(str(v).replace('|', '\\|').replace('\n', ' ') for v in cells) + ' |')
    return '\n'.join(lines) + '\n'


def inference_summary(rows):
    requests = [request for row in rows for request in row.get('metrics', {}).get('inference_requests', [])]
    complete = [r for r in requests if r.get('usage_complete')]
    def values(key):
        return [r.get('timings', {}).get(key) for r in complete if isinstance(r.get('timings'), dict)
                and isinstance(r['timings'].get(key), (int, float)) and math.isfinite(r['timings'][key])
                and r['timings'][key] >= 0]
    generation, prompt = values('predicted_per_second'), values('prompt_per_second')
    return {'requests': len(requests), 'complete_requests': len(complete),
            'generation_speed_samples': len(generation), 'prompt_speed_samples': len(prompt),
            'median_generation_tokens_per_second': statistics.median(generation) if generation else None,
            'median_prompt_tokens_per_second': statistics.median(prompt) if prompt else None}
