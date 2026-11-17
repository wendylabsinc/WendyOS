#!/usr/bin/env bash
# Reuse prepare-watch-container.sh output and the existing containerd build row.
set -euo pipefail
[[ $# == 1 && $(uname -s) == Linux ]] || { echo 'usage on Linux: prepare-multiapp-lifecycle.sh EXISTING_WATCH_OUTPUT' >&2; exit 2; }
output=$(cd "$1" && pwd -P)
[[ -x $output/normal && ! -e $output/early-readiness.json ]]
python3 - "$output" <<'PY'
import json
from pathlib import Path
import sys
out = Path(sys.argv[1])
readiness = Path('go/internal/cli/commands/readiness_observer.go').resolve()
compose = Path('go/internal/cli/commands/compose.go').resolve()
container = Path('go/internal/agent/containerd/client.go').resolve()
changes = {
    'early-readiness': (readiness, [('running := func(ctx context.Context) (bool, error) { return readinessState(ctx, conn, cfg) }', 'return err\n\trunning := func(ctx context.Context) (bool, error) { return readinessState(ctx, conn, cfg) }')]),
    'ignored-service-env': (compose, [('serviceEnvs[name] = mergeEnvEntries(expandServiceEnv(companion, nil), composeEnv(svc), expandServiceEnv(nil, serviceConfig), opts.env)', 'serviceEnvs[name] = mergeEnvEntries(expandServiceEnv(companion, nil), composeEnv(svc), expandServiceEnv(nil, serviceConfig))')]),
    'retained-isolation': (container, [('c.appIsolation[appID] = appCfg.Isolation', 'if appCfg.Isolation != "" { c.appIsolation[appID] = appCfg.Isolation }'), ('if _, loaded := c.appIsolation[appID]; loaded {', 'if c.appIsolation[appID] != "" {')]),
}
for name, (source, replacements) in changes.items():
    text = source.read_text()
    for before, after in replacements:
        assert text.count(before) == 1, name + ': production site changed'
        text = text.replace(before, after)
    mutant = out / (name + '.go')
    mutant.write_text(text)
    (out / (name + '.json')).write_text(json.dumps({'Replace': {str(source): str(mutant)}}))
PY
export CGO_ENABLED=1 GOOS=linux
for control in early-readiness ignored-service-env; do
  go build -overlay="$output/$control.json" -o "$output/$control" ./go/cmd/wendy
done
go test -overlay="$output/retained-isolation.json" -c -o "$output/retained-isolation.test" ./go/internal/agent/containerd
