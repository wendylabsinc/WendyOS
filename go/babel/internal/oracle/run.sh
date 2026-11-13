#!/bin/bash
# Run only inside a disposable, network-isolated, privileged Linux container.
# /reference is the pinned babeld build; /module is this module; /artifacts writable.
set -euo pipefail
mkdir -p /artifacts
go build -o /artifacts/babeltest ./cmd/babeltest
for n in left middle right; do
  ip netns add "$n"
  ip -n "$n" link set lo up
  ip netns exec "$n" sysctl -qw net.ipv6.conf.all.forwarding=1 net.ipv4.ip_forward=1
done
ip link add lm type veth peer name ml
ip link set lm netns left
ip link set ml netns middle
ip link add mr type veth peer name rm
ip link set mr netns middle
ip link set rm netns right
for pair in left:lm middle:ml middle:mr right:rm; do
  ns=${pair%:*}; dev=${pair#*:}
  ip -n "$ns" link set "$dev" addrgenmode none
  ip -n "$ns" link set "$dev" up
done
ip -n left addr add fe80::1/64 dev lm nodad
ip -n middle addr add fe80::2/64 dev ml nodad
ip -n middle addr add fe80::1/64 dev mr nodad
ip -n right addr add fe80::2/64 dev rm nodad
for pair in left:1 middle:2 right:3; do
  ns=${pair%:*}; id=${pair#*:}
  ip -n "$ns" addr add "fd00::$id/128" dev lo
  ip -n "$ns" addr add "192.0.2.$id/32" dev lo
done
ip netns exec middle /reference/babeld -d 1 -I /artifacts/babeld.pid -S /artifacts/babeld.state -g 33123 \
  -C 'interface ml type wired rxcost 96 hello-interval 1 update-interval 4' \
  -C 'interface mr type wired rxcost 96 hello-interval 1 update-interval 4' \
  -C 'redistribute local ip fd00::2/128 allow' \
  -C 'redistribute local ip 192.0.2.2/32 allow' \
  -C 'redistribute deny' ml mr > /artifacts/babeld.log 2>&1 &
bp=$!
ip netns exec middle tcpdump -U -i any -w /artifacts/control.pcap udp port 6696 > /artifacts/tcpdump.log 2>&1 &
tp=$!
ip netns exec left /artifacts/babeltest -id 1 -link lm,fe80::1,fe80::2 -originate fd00::1/128 -originate 192.0.2.1/32 -duration 45s > /artifacts/left.jsonl 2>/artifacts/left.err &
lp=$!
ip netns exec right /artifacts/babeltest -id 3 -link rm,fe80::2,fe80::1 -originate fd00::3/128 -originate 192.0.2.3/32 -withdraw-after 28s -duration 45s > /artifacts/right.jsonl 2>/artifacts/right.err &
rp=$!
trap 'kill "$bp" "$tp" "$lp" "$rp" 2>/dev/null || true' EXIT
ready=false
for i in $(seq 1 20); do
  if ip netns exec left ping -6 -c 1 -W 1 fd00::3 >/dev/null 2>&1 && ip netns exec left ping -4 -c 1 -W 1 192.0.2.3 >/dev/null 2>&1; then ready=true; break; fi
  sleep 1
done
"$ready" || { echo 'FAIL convergence'; exit 1; }
ip netns exec right ping -6 -c 2 -W 1 fd00::1
ip netns exec right ping -4 -c 2 -W 1 192.0.2.1
ip netns exec left ping -6 -c 2 -W 1 fd00::2
ip netns exec left ping -4 -c 2 -W 1 192.0.2.2
ip -n middle -6 route show > /artifacts/middle-ipv6.txt
ip -n middle -4 route show > /artifacts/middle-ipv4.txt
ip netns exec middle bash -c 'exec 3<>/dev/tcp/::1/33123; printf "dump\nquit\n" >&3; cat <&3' > /artifacts/babeld-dump.txt
wait "$lp"
wait "$rp"
# Withdrawal must cross the unmodified babeld and leave no usable exact route.
jq -s -e '.[-1].Routes | all(.Prefix != "fd00::3/128" or .Unreachable)' /artifacts/left.jsonl
jq -s -e '.[-1].Routes | all(.Prefix != "192.0.2.3/32" or .Unreachable)' /artifacts/left.jsonl
echo 'PASS mixed Go-babeld-Go IPv6 + IPv4-via-IPv6 forwarding and withdrawal'
