#!/bin/bash
# Reverse the oracle roles: an actual Go transit router between two babeld nodes.
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
pids=()
trap 'kill "${pids[@]}" 2>/dev/null || true' EXIT
for triple in left:1:lm right:3:rm; do
  IFS=: read -r ns id dev <<< "$triple"
  ip netns exec "$ns" /reference/babeld -d 1 -I "/artifacts/$ns.pid" -S "/artifacts/$ns.state" \
    -C "interface $dev type wired rxcost 96 hello-interval 1 update-interval 4" \
    -C "redistribute local ip fd00::$id/128 allow" \
    -C "redistribute local ip 192.0.2.$id/32 allow" \
    -C 'redistribute deny' "$dev" > "/artifacts/$ns.log" 2>&1 &
  pids+=("$!")
done
ip netns exec middle /artifacts/babeltest -id 2 -link ml,fe80::2,fe80::1 -link mr,fe80::1,fe80::2 \
  -originate fd00::2/128 -originate 192.0.2.2/32 -duration 55s > /artifacts/middle.jsonl 2>/artifacts/middle.err &
mp=$!; pids+=("$mp")
ip netns exec middle tcpdump -U -i any -w /artifacts/control.pcap udp port 6696 >/artifacts/tcpdump.log 2>&1 &
pids+=("$!")
ready=false
for i in $(seq 1 15); do
  if ip netns exec left ping -6 -c 1 -W 1 fd00::3 >/dev/null 2>&1 && ip netns exec left ping -4 -c 1 -W 1 192.0.2.3 >/dev/null 2>&1; then ready=true; break; fi
  sleep 1
done
"$ready" || { echo 'FAIL Go transit convergence'; exit 1; }
ip netns exec right ping -6 -c 2 -W 1 fd00::1
ip netns exec right ping -4 -c 2 -W 1 192.0.2.1
ip -n middle -6 route show > /artifacts/middle-ipv6.txt
ip -n middle -4 route show > /artifacts/middle-ipv4.txt
# Drop all traffic without administrative link notifications to test Hello/IHU expiry.
ip netns exec middle tc qdisc add dev mr root netem loss 100%
ip netns exec right tc qdisc add dev rm root netem loss 100%
sleep 16
jq -s -e '.[-1].Routes | any(.Prefix == "fd00::3/128" and .Unreachable)' /artifacts/middle.jsonl
ip netns exec middle tc qdisc del dev mr root
ip netns exec right tc qdisc del dev rm root
ready=false
for i in $(seq 1 15); do
  if ip netns exec left ping -6 -c 1 -W 1 fd00::3 >/dev/null 2>&1 && ip netns exec left ping -4 -c 1 -W 1 192.0.2.3 >/dev/null 2>&1; then ready=true; break; fi
  sleep 1
done
"$ready" || { echo 'FAIL Go transit recovery'; exit 1; }
wait "$mp"
echo 'PASS babeld-Go-babeld dual-family forwarding, silent loss and recovery'
