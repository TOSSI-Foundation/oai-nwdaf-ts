#!/bin/bash
# Start the 5G core, then replace the stock SMF with the steering build.
#
#   ./start_core.sh                 # asks which steering rule to use
#   ./start_core.sh --rule HEALTH   # non-interactive
#   ./start_core.sh --rule RATE
#
# Env:
#   FED         path to oai-cn5g-fed/docker-compose   (default $HOME/oai-cn5g-fed/docker-compose)
#   SMF_IMAGE   SMF image to run                      (default oai-smf:serialize)
set -u
FED=${FED:-$HOME/oai-cn5g-fed/docker-compose}

COMPOSE=${COMPOSE:-docker-compose-basic-vpp-pcf-steering.yaml}
SMF_IMAGE=${SMF_IMAGE:-oai-smf:serialize}
RULE=${SMF_NWDAF_DNPERF_RULE:-}
HERE=$(cd "$(dirname "$0")/../.." && pwd)

# ── preflight ────────────────────────────────────────────────────────────────
# Every check here failed silently or confusingly for someone once. A missing
# docker-compose used to surface as "sudo: docker-compose: command not found"
# half way through, after which nothing else in the bring-up could work but
# every later script still ran and reported its own unrelated timeouts.
#
# Both compose generations are accepted. v2 ('docker compose') is PREFERRED:
# v1.29.2 has the KeyError 'ContainerConfig' bug on locally-built images, and it
# kills the container before failing. Set COMPOSE_CMD to force one.
if [ -z "${COMPOSE_CMD:-}" ]; then
  if sudo docker compose version >/dev/null 2>&1;  then COMPOSE_CMD="docker compose"
  elif command -v docker-compose >/dev/null 2>&1;  then COMPOSE_CMD="docker-compose"
  else
    echo "FAIL: neither 'docker compose' (v2 plugin) nor 'docker-compose' (v1) is installed."
    echo "      Install one of:"
    echo "        sudo apt install -y docker-compose-plugin     # v2, recommended"
    echo "        sudo apt install -y docker-compose            # v1"
    exit 1
  fi
fi
echo "Compose: $COMPOSE_CMD"

[ -d "$FED" ] || { echo "FAIL: oai-cn5g-fed not found at $FED"
                   echo "      run ./scripts/build.sh fed   (or set FED)"; exit 1; }
[ -f "$FED/$COMPOSE" ] || { echo "FAIL: $COMPOSE not in $FED"
                            echo "      run ./scripts/build.sh fed"; exit 1; }
[ -f "$FED/database/oai_db2.sql" ] || { echo "FAIL: $FED/database/oai_db2.sql missing"
                                        echo "      without it no UE can authenticate"; exit 1; }
# Check the images the COMPOSE FILE names, not a hand-maintained list - the two
# drifted apart once already (compose pinned oai-smf:nwdaf-policy while build.sh
# produced oai-smf:serialize) and the hardcoded list happily passed.
#
# Only locally-built tags matter here: 'oai-*' or 'gnbsim*' with no registry
# namespace. Everything with a '/' (oaisoftwarealliance/...) and the official
# images (mysql, mongo) are pullable and compose will fetch them.
#
# A missing local tag is not a small error. Compose tries to PULL it, the pull is
# denied, and that aborts the PARALLEL pull for every other image too - so one
# absent tag surfaces as eight 'No such image' lines and hides its own cause.
miss=0
for i in $(grep -oP '^\s+image:\s*\K\S+' "$FED/$COMPOSE" | sort -u \
           | grep -Ev '/' | grep -E '^(oai-|gnbsim)'); do
  sudo docker image inspect "$i" >/dev/null 2>&1 \
    || { echo "FAIL: $COMPOSE needs image '$i', which is not present locally"; miss=1; }
done
# start_core.sh replaces the SMF with this one in step 2/2, so it must exist too.
sudo docker image inspect "$SMF_IMAGE" >/dev/null 2>&1 \
  || { echo "FAIL: missing image $SMF_IMAGE (SMF_IMAGE)"; miss=1; }
[ "$miss" = 0 ] || { echo "      run ./scripts/build.sh nfs   (a full C++ build, hours)"; exit 1; }

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
# REMOVE THE STANDALONE SMF FIRST - this is what makes the script idempotent.
#
# Step 2/2 below replaces the compose-created oai-smf with a standalone container
# carrying the NWDAF consumer. That container has no com.docker.compose.project
# label, so on the NEXT run compose does not recognise it as its own, tries to
# CREATE oai-smf, and the whole 'up' dies with
#     Conflict. The container name "/oai-smf" is already in use
# leaving every other NF up and the core half-started. The script therefore
# worked exactly once per machine and failed on every rerun.
#
# Removing it here costs nothing: step 2/2 recreates it a few seconds later.
sudo docker rm -f oai-smf >/dev/null 2>&1 || true

( cd "$FED" && sudo $COMPOSE_CMD -f "$COMPOSE" up -d ) || {
  echo "FAIL: compose could not bring the core up (see the error above)."
  echo "      If it names a container-name Conflict, that container was created"
  echo "      outside compose - remove it and rerun:  sudo docker rm -f <name>"
  exit 1; }
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
sudo FED="$FED" bash "$HERE/scripts/deploy/recreate_smf.sh" "$SMF_IMAGE" \
  -e SMF_NWDAF_ANALYTICS_ID=DN_PERFORMANCE \
  -e SMF_NWDAF_DNPERF_MARGIN_PERCENT="${SMF_NWDAF_DNPERF_MARGIN_PERCENT:-10}" \
  `# PREDICT_SEC>0 asks for PREDICTIONS, which carry a Confidence, and any DNAI` \
  `# under MIN_CONFIDENCE is dropped. Confidence tracks the analytics window and` \
  `# is not monotonic - measured on a 3-UE lab it rose 39->51 over ~5 min of` \
  `# traffic, fired the steer, then decayed back to ~35 and stayed there. RATE` \
  `# therefore has only a transient window in which it can act.` \
  `# SMF_NWDAF_PREDICT_SEC=0 selects STATISTICS, which carry no Confidence at` \
  `# all, so the floor never applies - that is the reliable way to exercise RATE.` \
  -e SMF_NWDAF_PREDICT_SEC="${SMF_NWDAF_PREDICT_SEC:-60}" \
  -e SMF_NWDAF_MIN_CONFIDENCE="${SMF_NWDAF_MIN_CONFIDENCE:-50}" \
  -e SMF_NWDAF_ACT="${SMF_NWDAF_ACT:-1}" \
  -e SMF_NWDAF_DNPERF_RULE="$RULE"
wait_healthy oai-smf 40
sudo docker exec vpp-upf /openair-upf/bin/vppctl show upf association 2>/dev/null | grep -q 'Node: oai-smf' \
  && echo "   ok: PFCP association up" \
  || echo "   WARNING: no PFCP association - check the SMF's --add-host for the UPF FQDN"

echo
echo "Core is up."
