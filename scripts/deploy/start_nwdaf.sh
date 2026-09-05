#!/bin/bash
# Start the NWDAF stack: database, engine, nbi-analytics, nbi-events,
# engine-traffic-steering. The SBI is deliberately NOT started here.
#
#   ./start_nwdaf.sh
#
# Env overrides: ENGINE_IMAGE ANALYTICS_IMAGE EVENTS_IMAGE STEERING_IMAGE
set -u
HERE=$(cd "$(dirname "$0")/../.." && pwd)

# 🔴 PIN THE IMAGE TAGS. nwdaf_stack_up.sh defaults to TAG=nwdaf-hardening, which
# predates the path-health work:
#   * a :nwdaf-hardening engine/analytics emits NO oaiPathHealthExt, so the HEALTH
#     rule sees no health at all, every DNAI reads UNKNOWN, and NOTHING EVER
#     STEERS - with no error anywhere to explain it;
#   * a :nwdaf-hardening sbi appends qosmonlist unbounded, so documents grow to
#     ~10 MB and the SMF starts timing out ("NWDAF returned HTTP 0").
export ENGINE_IMAGE=${ENGINE_IMAGE:-oai-nwdaf-engine:pathhealth}
export ANALYTICS_IMAGE=${ANALYTICS_IMAGE:-oai-nwdaf-nbi-analytics:pathhealth}
export EVENTS_IMAGE=${EVENTS_IMAGE:-oai-nwdaf-nbi-events:final}
export STEERING_IMAGE=${STEERING_IMAGE:-oai-nwdaf-engine-traffic-steering:latest}

for i in "$ENGINE_IMAGE" "$ANALYTICS_IMAGE" "$EVENTS_IMAGE" "$STEERING_IMAGE"; do
  sudo docker image inspect "$i" >/dev/null 2>&1 || { echo "missing image: $i"; exit 1; }
done

echo "──── NWDAF stack ────"
# On 192.168.75.0/24 deliberately: the shipped NWDAF compose uses 192.168.74.0/24,
# which is this deployment's SECONDARY N6 network. Creating it as shipped would
# collide with the path steering moves traffic onto.
sudo -E bash "$HERE/scripts/deploy/nwdaf_stack_up.sh"

echo
echo "   started:"
for c in oai-nwdaf-database oai-nwdaf-engine oai-nwdaf-nbi-analytics \
         oai-nwdaf-nbi-events oai-nwdaf-engine-traffic-steering; do
  printf "     %-36s %s\n" "$c" "$(sudo docker inspect $c --format '{{.Config.Image}}' 2>/dev/null || echo MISSING)"
done

echo
echo "──── host pollers ────"
# Both run OUTSIDE any container: the collector reads the UPF container's cgroup
# and vppctl; the route synchroniser rewrites oai-ext-dn's return routes so the
# DN answers on whichever N6 path a session was steered onto.
pgrep -f '[c]ollect_upf_metrics.py' >/dev/null && echo "   already running: telemetry collector" || {
  ( cd "$HERE" && sudo -n setsid scripts/telemetry/collect_upf_metrics.py --interval 5 >/dev/null 2>&1 & )
  echo "   started: telemetry collector"; }
pgrep -f '[n]wdaf_dn_route_sync.py' >/dev/null && echo "   already running: DN route sync" || {
  ( cd "$HERE" && sudo -n setsid python3 scripts/lab/nwdaf_dn_route_sync.py --interval 2 >/dev/null 2>&1 & )
  echo "   started: DN route sync"; }
sleep 4

echo
echo "──── check ────"
printf "   NBI latency: "; curl -sS -o /dev/null -w "%{time_total}s\n" -m 20 \
  "http://127.0.0.1:6059/nnwdaf-analyticsinfo/v1/analytics?event-id=DN_PERFORMANCE" 2>/dev/null || echo "unreachable"
# The first registration attempt often times out while the NRF is still coming
# up; the NBI retries every 10 s, so poll rather than reporting a single miss.
printf "   NWDAF in NRF: "
for i in $(seq 12); do
  r=$(curl --http2-prior-knowledge -s -m 5 \
      "http://192.168.70.130:8080/nnrf-nfm/v1/nf-instances?nf-type=NWDAF" \
      | python3 -c "import sys,json;print(','.join(i.get('href','') for i in json.load(sys.stdin).get('_links',{}).get('item',[])))" 2>/dev/null)
  [ -n "$r" ] && { echo "$r"; break; }
  sleep 5
done
[ -z "${r:-}" ] && echo "NOT REGISTERED after 60s - check oai-nwdaf-nbi-analytics logs"

echo
echo "NWDAF is up. Next: attach UEs -"
echo "  ./scripts/lab/demo_reset_multi.sh 5 1     # also starts the SBI, in order"
echo "then follow docs/MULTI-UE-STEERING.md"
