# Controller continuation through a slow gateway lease (audit 2524)

Use the existing Linux network job, with its iproute2, iptables, dnsmasq, ping
and kernel ip6tnl/conntrack support. No extra runner is needed:

```sh
python3 .github/scripts/prepare-sharing-slow-lease.py "$RUNNER_TEMP/sharing-slow-lease"
sudo --preserve-env=GITHUB_ACTIONS,RUNNER_ENVIRONMENT .github/scripts/test-sharing-slow-lease.sh \
  "$RUNNER_TEMP/sharing-slow-lease/normal.test" \
  "$RUNNER_TEMP/sharing-slow-lease/control.test" \
  "$RUNNER_TEMP/sharing-slow-lease"
```

Preserve the output directory. Both binaries are race-enabled; each compile has
180 seconds. Each runtime has a 110-second outer bound and a private mount,
network and PID namespace. Host `/run`, `/sys` and `/etc` state is hidden, except a read-only bind of
`/etc/alternatives` needed by Ubuntu iptables executable symlinks.
The two runtime cases normally require about 45–65 seconds combined, excluding
compilation. The outer bound also covers failure cleanup.

The real Controller keeps its unchanged five-second lease wait and a
15-second step context. A single owned TCP connection sends real TLS bytes,
then its first TLS response read waits 16 seconds. Three pending steps must
retain that connection, report offline/negotiating and avoid resolver selection.
The later step must become roaming with one dial. Existing real donor/transit/
borrower/app namespaces then require dual-stack HTTP, donor DNS, reserved-route
refusal, application grant/revocation and removal of the tunnel/defaults after
donor withdrawal. The control restores withdrawal during pending negotiation
and must fail specifically because the live TLS lease was cancelled.

Node directory and reachability-probe inputs are controlled. Resolver selection
is observed at the Controller policy boundary; this fixture does not run
systemd-resolved. The donor DNS exchange is a real UDP query. Physical BLE,
RF timing and real signed-advertisement propagation remain separate acceptance
requirements. Cross-compilation and `--prepare-only` are not runtime evidence.
