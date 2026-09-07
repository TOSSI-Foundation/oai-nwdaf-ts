#!/bin/bash
# SPDX-License-Identifier: LicenseRef-CSSL-1.0
# Recreate oai-smf on a given image, reproducing the running container's spec
# exactly (captured from `docker inspect oai-smf` before the swap).
#   sudo ./recreate_smf.sh oai-smf:dnperf [EXTRA -e ARGS...]
#
# Env:
#   FED   path to oai-cn5g-fed/docker-compose  (default $HOME/oai-cn5g-fed/docker-compose)
set -euo pipefail
IMAGE=${1:?usage: recreate_smf.sh <image> [extra -e docker args...]}
shift || true
FED=${FED:-$HOME/oai-cn5g-fed/docker-compose}
CONF=${SMF_CONFIG:-$FED/conf/ulcl_config.yaml}
[ -f "$CONF" ] || { echo "SMF config not found: $CONF" >&2
                    echo "run ./scripts/build.sh fed first, or set FED/SMF_CONFIG" >&2; exit 1; }

docker rm -f oai-smf >/dev/null 2>&1 || true
docker create --name oai-smf \
  --network demo-oai-public-net --ip 192.168.70.133 \
  -e TZ=Europe/Paris \
  -e SMF_NWDAF_ENABLE=1 \
  -e SMF_NWDAF_POLL_SEC=10 \
  -e SMF_NWDAF_LOAD_THRESHOLD=50 \
  -e SMF_NWDAF_CLEAR_STREAK=2 \
  -e SMF_NWDAF_DNAI_NORMAL=internet-primary \
  -e SMF_NWDAF_DNAI_CONGESTED=internet-secondary \
  "$@" \
  -v "$CONF":/openair-smf/etc/config.yaml \
  `# REQUIRED. ulcl_config.yaml addresses the UPF by FQDN and the UPF's N4` \
  `# address (192.168.70.201, what it registers in the NRF) is NOT its docker` \
  `# IP, so Docker's embedded DNS cannot resolve it. Without this the PFCP` \
  `# association never forms and no PDU session can be established.` \
  --add-host vpp-upf.node.5gcn.mnc95.mcc208.3gppnetwork.org:192.168.70.201 \
  --health-cmd '/openair-smf/bin/healthcheck.sh' \
  --health-interval 10s --health-timeout 15s --health-retries 6 \
  "$IMAGE" \
  /openair-smf/bin/oai_smf -c /openair-smf/etc/config.yaml -o >/dev/null
docker start oai-smf >/dev/null
echo "oai-smf recreated on $IMAGE"
