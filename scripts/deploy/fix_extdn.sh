#!/bin/bash
# Restore the oai-ext-dn baseline after any (re)deployment.
#
# WHY THIS IS NEEDED (test-environment dependency, NOT Route 2 logic):
# docker-compose-basic-vpp-pcf-steering.yaml hardcodes interface NAMES in the
# oai-ext-dn entrypoint:
#     ip route add 12.1.1.2/32 via 192.168.73.201 dev eth0
#     ip route add 12.1.1.3/32 via 192.168.74.201 dev eth1
#     iptables -t nat -A POSTROUTING -o eth0 -j MASQUERADE
# Docker does NOT guarantee which of the two attached networks becomes eth0 vs
# eth1, so on some deployments the routes fail (gateway not reachable on that
# interface) and the NAT rule binds the wrong interface. Symptoms: control
# plane perfect, but traceroute all '* * *' and 100% packet loss.
#
# This script is interface-order agnostic: it omits 'dev' so the kernel picks
# the interface from the gateway, and NATs both interfaces.
set -u
C=oai-ext-dn
echo "== $C interfaces =="
docker exec $C ip -o -4 addr show | awk '{print "  "$2" "$4}'
echo "== restoring UE return routes (no 'dev', order-agnostic) =="
docker exec $C ip route replace 12.1.1.2/32 via 192.168.73.201
docker exec $C ip route replace 12.1.1.3/32 via 192.168.74.201
docker exec $C ip route | grep 12.1.1 | sed 's/^/  /'
echo "== ensuring MASQUERADE on both interfaces =="
for i in eth0 eth1; do
  docker exec $C iptables -t nat -C POSTROUTING -o $i -j MASQUERADE 2>/dev/null \
    || docker exec $C iptables -t nat -A POSTROUTING -o $i -j MASQUERADE
done
docker exec $C iptables -t nat -L POSTROUTING -n -v | tail -3 | sed 's/^/  /'
echo "== done =="
