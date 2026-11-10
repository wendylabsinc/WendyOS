#!/usr/bin/env python3
"""Prepare an asciinema recording for the docs.

Usage: tighten_cast.py <in.cast> <out.cast> <options.json>

options.json keys, all optional:
  "until":    stop the recording at this second (source time)
  "squeeze":  [[start, end, seconds], ...] play these waits in `seconds`
              (source time); output inside them is merged to about 12 updates a second
  "idle":     longest pause to keep, default 2 seconds
  "replace":  [[old, new], ...] text replacements, for example a home folder
  "title":    title stored in the header

Source time is the time written in the input file. The output has its final
timing written in, so players need no idle limit.
"""
import json
import re
import sys

src, dst, options_path = sys.argv[1:4]
options = json.load(open(options_path))
idle = options.get('idle', 2.0)
until = options.get('until')
squeezes = sorted(options.get('squeeze', []))
replacements = options.get('replace', [])

lines = [l for l in open(src, encoding='utf-8').read().split('\n') if l.strip()]
header = json.loads(lines[0])
events = []
for line in lines[1:]:
    t, code, data = json.loads(line)
    if code != 'o' or (until is not None and t > until):
        continue
    events.append([t, data])

# Terminal queries are answered by the recording terminal, not the page, so drop them.
QUERIES = re.compile(r'\x1b\](?:10|11);\?(?:\x07|\x1b\\)|\x1b\[6n|\x1b\[>0?q|\x1b\[0?c|\x1b\[\?u|\x1b\[\?\d+\$p')
for event in events:
    text = QUERIES.sub('', event[1])
    for old, new in replacements:
        text = text.replace(old, new)
    event[1] = text
events = [e for e in events if e[1] != '']


def warp(t: float) -> float:
    """Map source time to time after the squeezes."""
    shift = 0.0
    for start, end, seconds in squeezes:
        if t >= end:
            shift += (end - start) - seconds
        elif t > start:
            shift += (t - start) * (1 - seconds / (end - start))
    return t - shift


def squeezed(t: float) -> bool:
    return any(start < t < end for start, end, _ in squeezes)


out = []
clock = 0.0   # output time
previous = 0.0  # warped time of the previous event
for t, data in events:
    w = warp(t)
    clock += min(w - previous, idle)
    previous = w
    if out and squeezed(t) and clock - out[-1][0] < 1 / 12:
        out[-1][1] += data  # merge fast updates inside a squeezed wait
    else:
        out.append([clock, data])

new_header = {'version': 2, 'width': header['width'], 'height': header['height']}
if options.get('title'):
    new_header['title'] = options['title']
with open(dst, 'w', encoding='utf-8') as f:
    f.write(json.dumps(new_header) + '\n')
    for clock, data in out:
        f.write(json.dumps([round(clock, 3), 'o', data], ensure_ascii=False) + '\n')

print(f'{len(events)} events over {events[-1][0]:.1f}s -> {len(out)} events over {out[-1][0]:.1f}s')
