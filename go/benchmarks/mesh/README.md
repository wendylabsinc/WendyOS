# Virtual mesh regression comparison

Run the production manifest protocol at two Git revisions without devices,
root privileges or real-time waits. The runner builds isolated detached
worktrees, injects the same fixture into each, alternates their execution order,
and records raw measurements and a comparison. It leaves the current branch
untouched. Python 3 and the repository's Go toolchain are required on macOS or
Linux.

For PR #3115:

```sh
python3 go/benchmarks/mesh/compare.py \
  --base 3dd9178f19fd6e9875656445ddb874ae8546f83c \
  --head 07addb59d3ef3da110c85a5ca2bedb165320e705 \
  --out /tmp/mesh-3115-comparison \
  --repeat 7 --benchtime 500ms
```

Fetch missing commits before running. Use a new output directory for each run.
The runner records commit IDs, toolchain, platform and fixture hashes in
`metadata.json`. It writes each execution's log, all numerical samples in
`samples.json`, and the comparison in both JSON and CSV. Test or build failures
stop the run; incomplete output is never reported as a passing comparison.

## What runs

The simulation uses production `Directory`, `Synchronizer`, certificate
exchange, signature verification, shared manifest pools, and `WriteControl` /
`ReadControl`. There are two small adapters for the announcement APIs before and
after PR #3115. They reproduce attach, change, queue-full retry, and periodic
maintenance calls. A future change to those scheduling rules needs a matching
adapter review, even if it still compiles.

The network has directed reliable FIFO links, 16 queued control frames per
direction, and a virtual serialization clock. The matrix includes a four-node
diamond, an eight-node line, and an eight-node full mesh. Each runs with both
1 MB/s plus 1 ms one-way propagation and 8 kB/s plus 20 ms propagation. These
are configured link models, not measured Ethernet or BLE characteristics.

Each topology runs:

- Cold convergence with initially unknown remote signing identities.
- Eleven minutes of steady operation, including fresh manifests from every
  origin at five-minute intervals and the old implementation's inventory sweeps.
- A burst of 32 revisions from every origin before links can drain. This exceeds
  each control queue's capacity and checks delivery of the latest revision.
- Reconnection of every carrier with warm directory and identity caches. This
  must not download known manifest bodies again.

Every phase checks that every node has every origin's latest revision. Probes
send 160-byte IPv6 packets over a fixed shortest path ten times per virtual
second, sharing each link's byte budget with control traffic. The report includes
their median, p95 and maximum modeled one-way latency. Convergence is the maximum
publication-to-admission delay among received revisions in that phase. Superseded
revisions may be coalesced; the final-revision assertion checks eventual delivery.

## Measurements and boundaries

| Metric | What it measures |
| --- | --- |
| Control frames and bytes | Serialized manifest and certificate messages, including the four-byte control length prefix; split by message kind |
| Probe datagrams and bytes | Carrier bodies along the fixed probe route, including the nine-byte carrier header |
| Convergence and probe latency | Virtual link delay, serialization and protocol maintenance scheduling |
| Publication benchmark | One changed origin propagated across a warm four-node diamond, including signing, verification, codecs and simulation bookkeeping |
| Inventory benchmark | Warm inventory production and serialization for 4, 32 and 128 origins through a 16-frame queue, without simulation event bookkeeping |
| Carrier benchmark | Production `carrierIPSender.sendIP` for 160-byte, whole 1320-byte and fragmented 1320-byte packets; uses the existing bounded mock of QUIC's owned packet copy |
| `allocs/op`, `B/op` | Go allocations and allocated bytes, excluding fixture setup |
| `cpu-ns/op`, `cpu-ns/B` | Process user plus system CPU from `getrusage`; one Go execution thread; CPU includes GC during the measured interval |

Control-frame counts are **not UDP packet counts**. The model excludes QUIC/TLS
headers, ACKs, retransmissions, congestion control, Babel Hello/Update traffic,
kernel/TUN forwarding, radio framing, contention, partitions and packet loss.
The probe scheduler models shared bandwidth, not QUIC's actual scheduling policy.
The carrier benchmark measures admission and framing, not end-to-end forwarding.
Publication benchmark allocations include the simulator; use the isolated
inventory and carrier benchmarks to distinguish those costs.

To measure total network packets and bytes, run the same workloads through real
carriers in a Linux network-namespace topology with `tc netem`, capture each link
once, and count UDP bytes and datagrams including ACKs and retransmissions. Add
loss, constrained bandwidth and concurrent application traffic there. These
virtual results do not substitute for that transport-level measurement.

## PR #3115 measurements

The pinned revisions above were compared on 2026-11-27 with Go 1.27.4 on an
Apple M4 Max, macOS arm64. Seven alternating rounds used 500 ms Go benchmark
targets and one Go execution thread. All topology and final-state assertions
passed in every round.

| Measurement | Observed change |
| --- | --- |
| Steady-state control frames across the matrix | 44–51% fewer |
| Steady-state control bytes | 28–41% fewer |
| Burst control frames | 40–45% fewer |
| Burst control bytes | 26–36% fewer |
| Cold and warm reconnect frame counts | Unchanged |
| One publication through a warm diamond, CPU | 1.427 ms to 1.538 ms, +7.8%; paired interval +6.3% to +9.3% |
| One publication, allocations | About 6,979 to 7,440, +6.6% |
| One publication, allocated bytes | About 563 kB to 599 kB, +6.4% |
| Inventory of 128 origins, CPU | 333.7 ms to 8.67 ms, 97.4% less |
| Inventory of 128 origins, allocations | About 1.94 million to 43,058, 97.8% fewer |
| Whole 1320-byte packet admission | One carrier datagram, 1329 carrier bytes and one allocation in both revisions |
| Fragmented 1320-byte packet admission | Two carrier datagrams, 1338 carrier bytes and two allocations in both revisions |

Carrier CPU intervals included zero change for all three packet sizes. Modeled
probe latency showed no increase during steady operation or bursts. For example,
the slow eight-node full mesh's maximum burst probe latency fell from 86.5 ms
to 49.4 ms; burst manifest convergence fell from 1.214 s to 1.128 s.

None of the 314 reported metrics exceeded the default 10% regression screen.
The smaller, repeatable increase in per-publication CPU and allocations is still
a tradeoff. These results support lower control traffic, not a claim that every
operation became cheaper.

## Regression screening

Each comparison reports means, percentage changes, and a paired bootstrap 95%
interval from 4,000 resamples. Seven alternating rounds are the default. A
regression is flagged only when the whole interval exceeds a 10% increase, or a
10% decrease for throughput. Newly nonzero metrics are flagged separately by
the same regression column. Use `--threshold` to change the budget and
`--fail-on-regression` to make flagged measurements return exit code 1.

This is an exploratory regression screen, not a guarantee of statistical
significance across hundreds of metrics. More rounds and a quiet dedicated host
are appropriate before enforcing CPU or latency gates in CI. Generated test
certificates have random signatures and slightly different encoded lengths;
small byte differences can alter delivery ordering. Exact cold and reconnect
frame counts, final-state correctness, and allocations are useful checks beside
the timing intervals.

Run the comparison parser checks with:

```sh
python3 -m unittest discover -s go/benchmarks/mesh -p 'test_*.py'
```
