#!/bin/bash
# Automated HEALTH-rule steering test.  PASS/FAIL, reproducible, self-cleaning.
#
#   ./scripts/test-health.sh [SECONDS] [RATE]
#     SECONDS  how long to wait for a steer after impairing the path (default 180)
#     RATE     tc token-bucket rate applied to the loaded path       (default 40mbit)
#
# WHAT IT PROVES. That degrading one N6 path causes the SMF to move a LIVE PDU
# session onto the other one, and that the move is real in the UPF's forwarding
# state - not merely printed in a log. Both are checked; the UPF is the verdict.
#
# WHAT IT DOES NOT PROVE. That the steer was optimal, or that the path health
# signal is packet loss. It is an AF_PACKET transmit-stall indicator specific to
# this VPP-on-veth lab. See README.md.
#
# PREREQUISITES, all checked before anything is impaired:
#   * the core, the NWDAF stack and >=2 UEs are up   (make core / nwdaf / ues)
#   * the SMF is running with SMF_NWDAF_DNPERF_RULE=HEALTH
#   * the host telemetry collector is running - WITHOUT IT THERE IS NO HEALTH AT
#     ALL, every DNAI reads UNKNOWN and the rule can never fire
#   * traffic is flowing, in UDP  (make load MBPS=60 PROTO=udp)
set -u
HERE=$(cd "$(dirname "$0")/.." && pwd)
. "$HERE/scripts/lib/upf_state.sh"

WAIT=${1:-180}
TCRATE=${2:-40mbit}
RC=0
IMPAIRED=""

pass(){ echo "  PASS  $*"; }
fail(){ echo "  FAIL  $*"; RC=1; }
info(){ echo "        $*"; }
hdr(){ echo; echo "──── $* ────"; }

# A leftover qdisc silently breaks every later test, so removal is unconditional
# and on every exit path, including Ctrl-C.
cleanup(){
  if [ -n "$IMPAIRED" ]; then
    sudo docker exec vpp-upf tc qdisc del dev "$IMPAIRED" root 2>/dev/null
    echo; echo "cleanup: removed the qdisc on $IMPAIRED -> $(sudo docker exec vpp-upf tc qdisc show dev "$IMPAIRED" | head -1)"
  fi
}
trap cleanup EXIT INT TERM

hdr "0. preflight"
for c in vpp-upf oai-smf oai-nwdaf-database oai-nwdaf-engine oai-nwdaf-nbi-analytics oai-nwdaf-sbi; do
  [ "$(sudo docker inspect -f '{{.State.Running}}' "$c" 2>/dev/null)" = true ] \
    && pass "$c running" || { fail "$c is not running"; }
done
[ $RC -eq 0 ] || { echo; echo "RESULT: FAIL (environment not up - run 'make up' first)"; exit 1; }

RULE=$(sudo docker inspect oai-smf --format '{{range .Config.Env}}{{println .}}{{end}}' \
       | sed -n 's/^SMF_NWDAF_DNPERF_RULE=//p')
[ "$RULE" = HEALTH ] && pass "SMF ranking rule is HEALTH" \
  || { fail "SMF ranking rule is '${RULE:-RATE}', not HEALTH"
       info "start the core with: make core RULE=HEALTH"; echo; echo "RESULT: FAIL"; exit 1; }

pgrep -f '[c]ollect_upf_metrics.py' >/dev/null \
  && pass "telemetry collector running" \
  || { fail "telemetry collector is NOT running - there will be no path health at all"
       info "start it with: ./scripts/deploy/start_nwdaf.sh"; echo; echo "RESULT: FAIL"; exit 1; }

SESS=$(upf_sessions)
N=$(echo "$SESS" | grep -c .)
[ "$N" -ge 2 ] && pass "$N PDU sessions established" \
  || { fail "need at least 2 PDU sessions, found $N"; echo; echo "RESULT: FAIL"; exit 1; }

hdr "1. baseline: where each session is, and how each path reads"
echo "$SESS" | awk '{printf "        SEID %-3s %-10s %s\n",$1,$2,$3}'
BEFORE=$(echo "$SESS" | awk '{print $1"="$3}' | sort | tr '\n' ' ')
upf_dnai_counts | awk '{printf "        %-22s %s session(s)\n",$1,$2}'
HEALTH0=$(path_health)
echo "$HEALTH0" | awk '{printf "        %-22s %-28s ratio=%-8s n=%s\n",$1,$2,$3,$4}'
[ "$HEALTH0" = NO-HEALTH-DATA ] && { fail "the collector has written no dnaiPerf at all"; echo; echo "RESULT: FAIL"; exit 1; }

# Impair whichever path is carrying the most sessions - that is the one a steer
# has somewhere to go FROM. Choosing it from live state rather than hardcoding
# 'internet-primary' means the test still works after a previous run moved
# everything the other way.
SRC=$(upf_dnai_counts | sort -k2 -rn | head -1 | awk '{print $1}')
DST=$(upf_dnai_counts | awk -v s="$SRC" '$1!=s{print $1}' | head -1)
[ -z "$DST" ] && DST="(the other authorized DNAI)"
IF_SRC=$(iface_of_dnai "$SRC")
[ -n "$IF_SRC" ] || { fail "cannot map $SRC to an N6 interface"; echo; echo "RESULT: FAIL"; exit 1; }
info "most-loaded path: $SRC on $IF_SRC  ->  expect sessions to move to $DST"

# A path with no traffic cannot report DEGRADED: with no transmit attempt,
# nothing can fail. Say so now rather than timing out in 3 minutes.
SRCATT=$(echo "$HEALTH0" | awk -v d="$SRC" '$1==d{print $4}')
if [ "${SRCATT:-0}" = 0 ] || [ "${SRCATT:-null}" = null ]; then
  fail "$SRC has 0 tx attempts - no traffic is flowing on it"
  info "an idle path is byte-for-byte identical to a healthy one; start load first:"
  info "  make load MBPS=60 SECS=1800 PROTO=udp"
  echo; echo "RESULT: FAIL (no traffic)"; exit 1
fi
pass "$SRC is carrying traffic ($SRCATT tx attempts in the last interval)"

hdr "2. impair $IF_SRC to $TCRATE"
sudo docker exec vpp-upf tc qdisc add dev "$IF_SRC" root tbf rate "$TCRATE" burst 64kbit latency 400ms \
  && IMPAIRED=$IF_SRC || { fail "could not apply tc qdisc"; echo; echo "RESULT: FAIL"; exit 1; }
info "$(sudo docker exec vpp-upf tc qdisc show dev "$IF_SRC" | head -1)"
T0=$(date +%s)

hdr "3. waiting up to ${WAIT}s for detection, decision and actuation"
SAW_DEGRADED=0; SAW_DECISION=0; SAW_ACTUATION=0; T_DEG=; T_DEC=; T_ACT=
while [ $(( $(date +%s) - T0 )) -lt "$WAIT" ]; do
  el=$(( $(date +%s) - T0 ))
  if [ $SAW_DEGRADED -eq 0 ] && path_health | grep -q "^$SRC OBSERVED_DEGRADED"; then
    SAW_DEGRADED=1; T_DEG=$el; pass "t+${el}s  NWDAF: $SRC = OBSERVED_DEGRADED"
    path_health | awk -v d="$SRC" '$1==d{printf "        ratio=%s over %s attempts\n",$3,$4}'
  fi
  if [ $SAW_DECISION -eq 0 ] && sudo docker logs oai-smf --since "${el}s" 2>&1 \
       | grep -q "SELECT '$DST'"; then
    SAW_DECISION=1; T_DEC=$el; pass "t+${el}s  SMF decided to select '$DST'"
  fi
  NOW=$(upf_sessions | awk '{print $1"="$3}' | sort | tr '\n' ' ')
  if [ $SAW_ACTUATION -eq 0 ] && [ "$NOW" != "$BEFORE" ]; then
    SAW_ACTUATION=1; T_ACT=$el; pass "t+${el}s  UPF forwarding state CHANGED"
  fi
  [ $SAW_ACTUATION -eq 1 ] && [ $SAW_DEGRADED -eq 1 ] && break
  sleep 5
done

# Same race as in test-rate.sh: the loop can break on actuation just before the
# decision line reaches the log. Re-check once over the whole window.
ELAPSED=$(( $(date +%s) - T0 + 30 ))
if [ $SAW_DECISION -eq 0 ] && sudo docker logs oai-smf --since "${ELAPSED}s" 2>&1 \
     | grep -q "SELECT '$DST'"; then SAW_DECISION=1; T_DEC="<=$((ELAPSED-30))"; fi

hdr "4. result"
AFTER=$(upf_sessions)
echo "$AFTER" | awk '{printf "        SEID %-3s %-10s %s\n",$1,$2,$3}'
MOVED=0
for kv in $BEFORE; do
  seid=${kv%%=*}; was=${kv#*=}
  now=$(echo "$AFTER" | awk -v s="$seid" '$1==s{print $3}')
  [ -n "$now" ] && [ "$now" != "$was" ] && { MOVED=$((MOVED+1)); info "SEID $seid: $was -> $now"; }
done

[ $SAW_DEGRADED  -eq 1 ] && pass "detection:  $SRC reported OBSERVED_DEGRADED (t+${T_DEG}s)" \
                         || fail "detection:  $SRC never reported OBSERVED_DEGRADED in ${WAIT}s"
[ $SAW_DECISION  -eq 1 ] && pass "decision:   SMF selected '$DST' (t+${T_DEC}s)" \
                         || fail "decision:   the SMF never logged a selection of '$DST'"
[ $MOVED -gt 0 ] && pass "actuation:  $MOVED session(s) moved off $SRC in the UPF (t+${T_ACT:-?}s)" \
                 || fail "actuation:  no session changed network instance in the UPF"

# Serialization is part of the contract: at most one steer per evaluation cycle.
MAXCYC=$(sudo docker logs oai-smf --since "$((WAIT+30))s" 2>&1 \
         | grep -oP 'Steering cycle: \d+ eligible session\(s\), \K\d+' | sort -rn | head -1)
[ -n "$MAXCYC" ] && { [ "$MAXCYC" -le 1 ] \
  && pass "serialization: at most $MAXCYC steer per cycle" \
  || fail "serialization: $MAXCYC steers in one cycle (expected <=1)"; }

echo
if [ $RC -eq 0 ]; then echo "RESULT: PASS - HEALTH steering detected, decided and actuated"
else echo "RESULT: FAIL"; fi
exit $RC
