#!/bin/bash
# Start the 5G core, then replace the stock SMF with the steering build.
#
#   ./start_core.sh                 # asks which steering rule to use
#   ./start_core.sh --rule HEALTH   # non-interactive
#   ./start_core.sh --rule RATE
#
# Env:
#   FED         path to oai-cn5g-fed/docker-compose   (default /home/ubuntu/oai-cn5g-fed/docker-compose)
#   SMF_IMAGE   SMF image to run                      (default oai-smf:serialize)
set -u
FED=${FED:-/home/ubuntu/oai-cn5g-fed/docker-compose}
COMPOSE=${COMPOSE:-docker-compose-basic-vpp-pcf-steering.yaml}
SMF_IMAGE=${SMF_IMAGE:-oai-smf:serialize}
RULE=${SMF_NWDAF_DNPERF_RULE:-}
HERE=$(cd "$(dirname "$0")/../.." && pwd)

while [ $# -gt 0 ]; do case "$1" in --rule) RULE=$2; shift 2;; *) shift;; esac; done

if [ -z "$RULE" ]; then
  cat <<'ASK'

Which steering decision rule should the SMF use?

  1) RATE    argmax(avgTrafficRate) over the DNAIs the PCF authorized.
             Ranks on OFFERED LOAD, so it steers TOWARD the busier path and
             cannot return to an idle one. It is the original behaviour and the
             default if nothing is set. Input field is 3GPP (Table 6.14.3-1);
             the ranking rule itself is project-specific.

  2) HEALTH  Gates on per-DNAI path health. Steers only AWAY from a path
             observed degraded, holds on healthy and on every UNKNOWN state,
             and is reversible. Input is the vendor extension oaiPathHealthExt.

ASK
  read -r -p "Choose [1/2] (default 2): " a
  case "${a:-2}" in 1) RULE=RATE;; *) RULE=HEALTH;; esac
fi
case "$(echo "$RULE" | tr a-z A-Z)" in
  RATE|HEALTH) RULE=$(echo "$RULE" | tr a-z A-Z);;
  *) echo "rule must be RATE or HEALTH (got '$RULE')"; exit 1;;
esac
echo "Steering rule: $RULE"

wait_healthy(){ for i in $(seq "${2:-40}"); do
  [ "$(sudo docker inspect -f '{{.State.Health.Status}}' "$1" 2>/dev/null)" = healthy ] && { echo "   ok: $1"; return 0; }
  sleep 3; done; echo "   TIMEOUT: $1"; return 1; }

echo
echo "──── 1/2  core NFs ────"
# NEVER 'docker-compose down -v'. The NWDAF MongoDB is on an anonymous volume and
# -v destroys every metric ever collected.
#
# docker-compose v1 KILLS the container before failing with KeyError
# 'ContainerConfig' on locally-built images. If that happens, recreate the
# affected container by hand from a saved 'docker inspect', replaying networks,
# env, entrypoint AND HostConfig.PortBindings.
( cd "$FED" && sudo docker-compose -f "$COMPOSE" up -d ) || exit 1
for c in mysql oai-nrf oai-amf oai-ausf oai-udm oai-udr oai-pcf vpp-upf oai-ext-dn; do
  wait_healthy "$c" 40 || true
done

echo
echo "──── 2/2  SMF: $SMF_IMAGE, rule=$RULE ────"
# The compose file ships the UPSTREAM SMF. Ours carries the NWDAF consumer, the
# DNAI steering actuation and the HEALTH rule, so it is recreated standalone.
#
# The SBI is removed first and NOT restarted here: it must come up after the SMF
# is stable and before any UE attaches, which start_ues / demo_reset_multi.sh do.
sudo docker rm -f oai-nwdaf-sbi >/dev/null 2>&1
sudo bash "$HERE/scripts/deploy/recreate_smf.sh" "$SMF_IMAGE" \
  -e SMF_NWDAF_ANALYTICS_ID=DN_PERFORMANCE \
  -e SMF_NWDAF_DNPERF_MARGIN_PERCENT=10 \
  -e SMF_NWDAF_PREDICT_SEC=60 \
  -e SMF_NWDAF_MIN_CONFIDENCE=50 \
  -e SMF_NWDAF_ACT=1 \
  -e SMF_NWDAF_DNPERF_RULE="$RULE"
wait_healthy oai-smf 40
sudo docker exec vpp-upf /openair-upf/bin/vppctl show upf association 2>/dev/null | grep -q 'Node: oai-smf' \
  && echo "   ok: PFCP association up" \
  || echo "   WARNING: no PFCP association - check the SMF's --add-host for the UPF FQDN"

echo
echo "Core is up. Next:  ./scripts/deploy/start_nwdaf.sh"
