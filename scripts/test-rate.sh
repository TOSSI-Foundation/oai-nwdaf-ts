#!/bin/bash
# Automated RATE-rule steering test.  PASS/FAIL, reproducible.
#
#   ./scripts/test-rate.sh [SECONDS]
#     SECONDS  how long to wait for a steer (default 240)
#
# WHAT THE RATE RULE IS. argmax(avgTrafficRate) over the DNAIs the PCF authorized
# for that session. avgTrafficRate is OFFERED LOAD, not path quality, so the rule
# steers TOWARD the busier path. That is not a bug in this test - it is the
# behaviour, and it is why HEALTH exists. This test asserts exactly that
# behaviour, because a rule that silently stopped doing it would be a regression.
#
# HOW IT IS TRIGGERED. Load the ANCHOR UE (pinned to internet-secondary) and
# leave the steerable UEs on internet-primary comparatively idle. Secondary then
# has the higher avgTrafficRate and the primary sessions should migrate onto it.
# Unlike HEALTH this needs no impairment, so nothing has to be undone afterwards.
#
# TIMING. The engine averages DN_PERFORMANCE over ENGINE_DN_PERFORMANCE_WINDOW_SEC
# (300 s by default), so the rate separation has to build up. A short run can
# legitimately time out; the default wait is deliberately longer than HEALTH's.
set -u
HERE=$(cd "$(dirname "$0")/.." && pwd)
. "$HERE/scripts/lib/upf_state.sh"

WAIT=${1:-240}
RC=0
pass(){ echo "  PASS  $*"; }
fail(){ echo "  FAIL  $*"; RC=1; }
info(){ echo "        $*"; }
hdr(){ echo; echo "──── $* ────"; }

hdr "0. preflight"
for c in vpp-upf oai-smf oai-nwdaf-database oai-nwdaf-engine oai-nwdaf-nbi-analytics oai-nwdaf-sbi; do
  [ "$(sudo docker inspect -f '{{.State.Running}}' "$c" 2>/dev/null)" = true ] \
    && pass "$c running" || fail "$c is not running"
done
[ $RC -eq 0 ] || { echo; echo "RESULT: FAIL (environment not up - run 'make up' first)"; exit 1; }

RULE=$(sudo docker inspect oai-smf --format '{{range .Config.Env}}{{println .}}{{end}}' \
       | sed -n 's/^SMF_NWDAF_DNPERF_RULE=//p')
RULE=${RULE:-RATE}     # RATE is the shipping default when the variable is unset
[ "$RULE" = RATE ] && pass "SMF ranking rule is RATE" \
  || { fail "SMF ranking rule is '$RULE', not RATE"
       info "start the core with: make core RULE=RATE"; echo; echo "RESULT: FAIL"; exit 1; }

SESS=$(upf_sessions); N=$(echo "$SESS" | grep -c .)
[ "$N" -ge 2 ] && pass "$N PDU sessions established" \
  || { fail "need at least 2 PDU sessions, found $N"; echo; echo "RESULT: FAIL"; exit 1; }

hdr "1. baseline"
echo "$SESS" | awk '{printf "        SEID %-3s %-10s %s\n",$1,$2,$3}'
BEFORE=$(echo "$SESS" | awk '{print $1"="$3}' | sort | tr '\n' ' ')
upf_dnai_counts | awk '{printf "        %-22s %s session(s)\n",$1,$2}'

# The rule needs two DNAIs with DIFFERENT rates and at least one session sitting
# on the slower one. If every session is already on the busier path there is
# nothing left to steer and the correct answer is "nothing happened".
SRC=$(upf_dnai_counts | sort -k2 -rn | head -1 | awk '{print $1}')
DST=$(upf_dnai_counts | awk -v s="$SRC" '$1!=s{print $1}' | head -1)
if [ -z "$DST" ]; then
  fail "all $N sessions are already on $SRC - there is no second path to compare"
  info "reset the lab so the steerable UEs start on primary and one anchor holds"
  info "secondary:  ./scripts/lab/demo_reset_multi.sh 3 1"
  echo; echo "RESULT: FAIL (degenerate topology)"; exit 1
fi

hdr "2. what DN_PERFORMANCE reports per DNAI"
# Read the analytic the SMF actually consumes, over its own NBI - not the
# database behind it. If this is empty the SMF has nothing to rank and the test
# would otherwise time out with no explanation.
ANA=$(curl -sS -m 25 "http://127.0.0.1:6059/nnwdaf-analyticsinfo/v1/analytics?event-id=DN_PERFORMANCE" 2>/dev/null)
if [ -z "$ANA" ]; then
  fail "the analytics NBI returned nothing on :6059"
  echo; echo "RESULT: FAIL"; exit 1
fi
# The real nesting is dnPerfInfos[].dnPerf[].perfData.avgTrafficRate, and the
# rate is a STRING with a unit ("0 bps", "123.90 Mbps") - not a number.
RATES=$(echo "$ANA" | python3 -c '
import sys, json
try: d = json.load(sys.stdin)
except Exception: sys.exit()
for info in d.get("dnPerfInfos") or []:
    for e in info.get("dnPerf") or []:
        dnai = e.get("dnai")
        raw  = (e.get("perfData") or {}).get("avgTrafficRate")
        if not dnai: continue
        bps = 0.0
        if isinstance(raw, str):
            parts = raw.split()
            try: v = float(parts[0])
            except (ValueError, IndexError): v = 0.0
            unit = (parts[1] if len(parts) > 1 else "bps").lower()
            bps = v * {"bps":1, "kbps":1e3, "mbps":1e6, "gbps":1e9}.get(unit, 1)
        elif isinstance(raw, (int, float)):
            bps = float(raw)
        print(f"{dnai} {bps:.0f} {raw}")' 2>/dev/null)
echo "$RATES" | awk '{printf "        %-22s avgTrafficRate=%s\n",$1,$3" "$4}'

# The DNAI the RATE rule should converge on: argmax(avgTrafficRate).
BUSIEST=$(echo "$RATES" | sort -k2 -rn | head -1 | awk '{print $1}')
[ -n "$BUSIEST" ] && info "argmax(avgTrafficRate) = $BUSIEST - sessions should converge here"

hdr "3. waiting up to ${WAIT}s for a RATE-driven steer"
info "expecting sessions to move TOWARD the higher-rate DNAI"
T0=$(date +%s); SAW_DECISION=0; SAW_ACTUATION=0; T_DEC=; T_ACT=
while [ $(( $(date +%s) - T0 )) -lt "$WAIT" ]; do
  el=$(( $(date +%s) - T0 ))
  if [ $SAW_DECISION -eq 0 ] && sudo docker logs oai-smf --since "$((el+15))s" 2>&1 \
       | grep -qE "per-session decision.*SELECT"; then
    SAW_DECISION=1; T_DEC=$el
    pass "t+${el}s  SMF logged a per-session selection"
    sudo docker logs oai-smf --since "$((el+15))s" 2>&1 | grep -oP "SELECT '\K[^']+" \
      | sort -u | sed 's/^/        selected: /'
  fi
  NOW=$(upf_sessions | awk '{print $1"="$3}' | sort | tr '\n' ' ')
  if [ $SAW_ACTUATION -eq 0 ] && [ "$NOW" != "$BEFORE" ]; then
    SAW_ACTUATION=1; T_ACT=$el; pass "t+${el}s  UPF forwarding state CHANGED"; break
  fi
  sleep 5
done

# The loop can break on actuation microseconds before the decision line is
# flushed to the log, so make the authoritative check ONCE, afterwards, over the
# whole elapsed window. Polling mid-loop is only there to timestamp it.
ELAPSED=$(( $(date +%s) - T0 + 30 ))
if [ $SAW_DECISION -eq 0 ] && sudo docker logs oai-smf --since "${ELAPSED}s" 2>&1 \
     | grep -qE "per-session decision.*SELECT"; then
  SAW_DECISION=1; T_DEC="<=$((ELAPSED-30))"
fi

hdr "4. result"
AFTER=$(upf_sessions)
echo "$AFTER" | awk '{printf "        SEID %-3s %-10s %s\n",$1,$2,$3}'
MOVED=0; TOWARD=0
for kv in $BEFORE; do
  seid=${kv%%=*}; was=${kv#*=}
  now=$(echo "$AFTER" | awk -v s="$seid" '$1==s{print $3}')
  if [ -n "$now" ] && [ "$now" != "$was" ]; then
    MOVED=$((MOVED+1)); info "SEID $seid: $was -> $now"
    [ "$now" = "${BUSIEST:-$SRC}" ] && TOWARD=$((TOWARD+1))
  fi
done

[ $SAW_DECISION -eq 1 ] && pass "decision:   the SMF ranked and selected a DNAI (t+${T_DEC}s)" \
                        || fail "decision:   the SMF logged no per-session selection in ${WAIT}s"
[ $MOVED -gt 0 ] && pass "actuation:  $MOVED session(s) changed network instance in the UPF (t+${T_ACT:-?}s)" \
                 || fail "actuation:  no session moved"
if [ $MOVED -gt 0 ] && [ -n "${BUSIEST:-}" ]; then
  [ $TOWARD -gt 0 ] \
    && pass "direction:  moved TOWARD the highest-rate DNAI ($BUSIEST) - the documented RATE behaviour" \
    || fail "direction:  moved AWAY from the highest-rate DNAI ($BUSIEST) - RATE should steer toward it"
fi

echo
if [ $RC -eq 0 ]; then echo "RESULT: PASS - RATE steering decided and actuated"
else echo "RESULT: FAIL"; fi
exit $RC
