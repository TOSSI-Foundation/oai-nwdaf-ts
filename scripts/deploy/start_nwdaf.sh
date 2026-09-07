#!/bin/bash
# Start the NWDAF stack: database, engine, nbi-analytics, nbi-events,
# engine-traffic-steering and the SBI.
#
# NOTE ON THE SBI. nwdaf_stack_up.sh starts it here, which is safe because the
# SMF is already up by this point. demo_reset_multi.sh then REMOVES and recreates
# it before attaching UEs - deliberately, because the ordering constraint is
# strict: the SBI must come up after the SMF is stable and before any UE
# attaches. Too early duplicates its subscription and every usage report is
# counted twice; too late and the UP_PATH_CH events that record which DNAI a
# session is on are emitted with no subscriber and lost, after which
# DN_PERFORMANCE attributes usage to a stale DNAI forever.
# So the SBI being started twice across a full bring-up is expected, not a bug.
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
# The SBI must be pinned too. nwdaf_stack_up.sh otherwise defaults it to
# :nwdaf-hardening, whose unbounded qosmonlist grows documents to ~10 MB until
# the SMF times out ("NWDAF returned HTTP 0").
export SBI_IMAGE=${SBI_IMAGE:-oai-nwdaf-sbi:qosmon-retain}

# OPTIONAL. oai-nwdaf-engine-traffic-steering is the ML congestion-forecast
# component (Track 1). It is NOT on the RATE or HEALTH steering path and its
# source and models are deliberately not part of this repository, so its absence
# must not be fatal - it used to abort the whole bring-up here.
export STEERING_IMAGE=${STEERING_IMAGE:-oai-nwdaf-engine-traffic-steering:latest}
if ! sudo docker image inspect "$STEERING_IMAGE" >/dev/null 2>&1; then
  echo "   note: $STEERING_IMAGE not present - skipping the optional ML"
  echo "         congestion-forecast engine. RATE and HEALTH steering do not use it."
  export SKIP_STEERING_ENGINE=1
fi

missing=0
for i in "$ENGINE_IMAGE" "$ANALYTICS_IMAGE" "$EVENTS_IMAGE" "$SBI_IMAGE"; do
  sudo docker image inspect "$i" >/dev/null 2>&1 || { echo "missing image: $i"; missing=1; }
done
[ "$missing" = 0 ] || { echo; echo "Build them first:  ./scripts/build.sh nwdaf"; exit 1; }

# The NWDAF engine, analytics and SBI all attach to the CONTROL-PLANE network as
# well, so the core must already exist. Without this check nwdaf_stack_up.sh gets
# as far as 'docker network connect', dies under set -e leaving the stack half
# built, and the failure reads as an NWDAF problem rather than "the core is not up".
for n in demo-oai-public-net; do
  sudo docker network inspect "$n" >/dev/null 2>&1 || {
    echo "FAIL: network '$n' does not exist - the 5G core is not running."
    echo "      Run ./scripts/deploy/start_core.sh first, and check it succeeded."
    exit 1; }
done

echo "──── NWDAF stack ────"
# On 192.168.75.0/24 deliberately: the shipped NWDAF compose uses 192.168.74.0/24,
# which is this deployment's SECONDARY N6 network. Creating it as shipped would
# collide with the path steering moves traffic onto.
sudo -E bash "$HERE/scripts/deploy/nwdaf_stack_up.sh" || {
  echo; echo "FAIL: nwdaf_stack_up.sh aborted - the stack is only partly up."
  echo "      Fix the error above, then: make clean && make core && make nwdaf"
  exit 1; }

echo
echo "   started:"
LIST="oai-nwdaf-database oai-nwdaf-engine oai-nwdaf-nbi-analytics oai-nwdaf-nbi-events oai-nwdaf-sbi"
[ -z "${SKIP_STEERING_ENGINE:-}" ] && LIST="$LIST oai-nwdaf-engine-traffic-steering"
missing=0
for c in $LIST; do
  img=$(sudo docker inspect "$c" --format '{{.Config.Image}}' 2>/dev/null)
  [ -n "$img" ] || { img=MISSING; missing=1; }
  printf "     %-36s %s\n" "$c" "$img"
done
[ "$missing" = 0 ] || { echo
  echo "FAIL: not every NWDAF container started. Do not continue to 'make ues' -"
  echo "      it will report unrelated timeouts. Fix the cause above first."
  exit 1; }

echo
echo "──── host pollers ────"
# Both run OUTSIDE any container: the collector reads the UPF container's cgroup
# and vppctl; the route synchroniser rewrites oai-ext-dn's return routes so the
# DN answers on whichever N6 path a session was steered onto.
# The collector imports pymongo, which is NOT in the system interpreter. It
# used to be launched as a bare ./script.py with stderr sent to /dev/null and
# "started" printed unconditionally, so an exec failure or a missing pymongo was
# completely silent - and with no collector there is no path health at all, every
# DNAI reads UNKNOWN and the HEALTH rule never steers. Build a venv, then CHECK.
VENV=${VENV:-$HERE/.venv}
if [ ! -x "$VENV/bin/python3" ]; then
  echo "   creating $VENV"
  python3 -m venv "$VENV" >/dev/null 2>&1 || { echo "   FAILED: python3 -m venv (apt install python3-venv)"; exit 1; }
  "$VENV/bin/pip" install -q -r "$HERE/scripts/telemetry/requirements.txt" \
    || { echo "   FAILED: pip install -r scripts/telemetry/requirements.txt"; exit 1; }
fi
if pgrep -f '[c]ollect_upf_metrics.py' >/dev/null; then
  echo "   already running: telemetry collector"
else
  ( cd "$HERE" && sudo -n setsid "$VENV/bin/python3" scripts/telemetry/collect_upf_metrics.py \
      --interval 5 >"$HERE/.collector.log" 2>&1 & )
  sleep 3
  if pgrep -f '[c]ollect_upf_metrics.py' >/dev/null; then
    echo "   started: telemetry collector"
  else
    echo "   FAILED to start the telemetry collector - without it NOTHING EVER STEERS:"
    tail -5 "$HERE/.collector.log" 2>/dev/null | sed 's/^/     /'
    exit 1
  fi
fi
if pgrep -f '[n]wdaf_dn_route_sync.py' >/dev/null; then
  echo "   already running: DN route sync"
else
  ( cd "$HERE" && sudo -n setsid python3 scripts/lab/nwdaf_dn_route_sync.py \
      --interval 2 >"$HERE/.routesync.log" 2>&1 & )
  sleep 2
  pgrep -f '[n]wdaf_dn_route_sync.py' >/dev/null \
    && echo "   started: DN route sync" \
    || { echo "   FAILED to start the DN route sync - a steer will break connectivity:"
         tail -5 "$HERE/.routesync.log" 2>/dev/null | sed 's/^/     /'; exit 1; }
fi
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
echo "NWDAF is up. Next: attach UEs (this recreates the SBI in order) -"
echo "  ./scripts/lab/demo_reset_multi.sh 5 1     # also starts the SBI, in order"
echo "then follow docs/MULTI-UE-STEERING.md"
