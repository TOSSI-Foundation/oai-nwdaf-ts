#!/bin/bash
# SPDX-License-Identifier: LicenseRef-CSSL-1.0
#
# Recreate a gnbsim UE, by name.
#
#   sudo ./recreate_gnbsim.sh                # gnbsim-vpp2  (the default UE)
#   sudo ./recreate_gnbsim.sh gnbsim-vpp3    # the second UE
#   sudo ./recreate_gnbsim.sh gnbsim-vpp3 --keep   # no-op if already attached
#
# gnbsim can only be RECREATED, never restarted: its entrypoint renders @VAR@
# placeholders with `sed -i`, so a second run finds nothing to substitute and
# aborts under `set -euo pipefail`.
# create -> connect BOTH networks -> start (the second network must be attached
# before the process runs, or GTPuLocalAddr does not exist yet).
#
# WHY THIS IS A TABLE AND NOT A FORKED SCRIPT (PROJECT-HISTORY section 23)
# The second UE was originally created by copying this file and editing three
# values, and shipped with GNBID=5 - IDENTICAL to gnbsim-vpp2. GNBID is the
# Global gNB ID: two gNBs presenting the same one are one gNB re-registering as
# far as the AMF is concerned. Keeping every UE in one table is what makes such
# a collision visible.
#
# RANUENGAPID IS DELIBERATELY 0 FOR EVERY UE - DO NOT "FIX" IT.
# *Classification: deployment/test-environment limitation (gnbsim), not 5GC.*
# It is tempting to give each UE a distinct one; it does not work and it is not
# needed:
#   * NOT NEEDED. TS 38.413 clause 9.3.3.2 scopes the RAN UE NGAP ID to "the UE
#     association over the NG interface WITHIN the NG-RAN node". Each gnbsim is
#     a separate NG-RAN node with its own SCTP association, so both may use 0.
#   * DOES NOT WORK. This gnbsim build registers its single "camper" under a
#     RAN UE NGAP ID of 0 regardless of what the config file says, while sending
#     the configured value on the wire. The AMF correctly echoes the configured
#     value back in DownlinkNASTransport, LookupCamperByRanId() then misses, and
#     SendtoUE() dereferences nil. Measured: RANUENGAPID=1 -> "cannot find
#     camper for RanId=1" + SIGSEGV; RANUENGAPID=2 -> the same at RanId=2;
#     RANUENGAPID=0 -> attaches (UE address 12.1.1.3). The panic lands on the
#     first Authentication Request, so it reads like an authentication failure
#     and sends you to the AMF or the subscriber database, where nothing is
#     wrong - the AUSF has already issued vectors by then.
#
# Anything here can be overridden from the environment for a one-off UE, e.g.
#   MSIN=0000000034 CPIP=192.168.70.144 GTPU=192.168.72.144 GNBID=7 \
#   RANUENGAPID=2 NRCELLID=3 sudo ./recreate_gnbsim.sh gnbsim-vpp4
set -euo pipefail

NAME=${1:-gnbsim-vpp2}
KEEP=${2:-}

# Every field must be unique per UE EXCEPT RANUENGAPID, which is 0 for all of
# them - see the header. MSIN must be provisioned in mysql or the UE will not
# authenticate.
# name           CP IP            GTP-U IP         MSIN        GNBID NRCellID
read -r -d '' UE_TABLE <<'TABLE' || true
gnbsim-vpp2      192.168.70.142   192.168.72.142   0000000032  5     1
gnbsim-vpp3      192.168.70.143   192.168.72.143   0000000033  6     2
TABLE

row=$(awk -v n="$NAME" '$1==n {print; exit}' <<<"$UE_TABLE")
if [ -z "$row" ] && [ -z "${MSIN:-}" ]; then
  echo "unknown UE '$NAME'. Known:" >&2
  awk 'NF {print "  " $1}' <<<"$UE_TABLE" >&2
  echo "  (or set MSIN/CPIP/GTPU/GNBID/RANUENGAPID/NRCELLID to define a new one)" >&2
  exit 2
fi
# shellcheck disable=SC2086
set -- $row
CPIP=${CPIP:-${2:-}}; GTPU=${GTPU:-${3:-}}; MSIN=${MSIN:-${4:-}}
GNBID=${GNBID:-${5:-}}; NRCELLID=${NRCELLID:-${6:-}}
# Overridable only so the finding above can be re-demonstrated on demand.
RANUENGAPID=${RANUENGAPID:-0}

for v in CPIP GTPU MSIN GNBID NRCELLID; do
  [ -n "${!v}" ] || { echo "$NAME: $v is not set" >&2; exit 2; }
done

if [ "$RANUENGAPID" != "0" ]; then
  echo "WARNING: RANUENGAPID=$RANUENGAPID. This gnbsim build panics on the first" >&2
  echo "         DownlinkNASTransport for any value but 0 (see the header)." >&2
fi

# GNBID, the UE IPs and the MSIN must be unique across every RUNNING gnbsim.
# Checked against what is actually RUNNING, not against the table, so an
# environment override cannot silently reintroduce a collision.
for other in $(docker ps --format '{{.Names}}' | grep '^gnbsim-' | grep -v "^${NAME}$" || true); do
  oenv=$(docker inspect "$other" --format '{{range .Config.Env}}{{println .}}{{end}}' 2>/dev/null || true)
  for pair in "GNBID:$GNBID" "MSIN:$MSIN" "GTPuLocalAddr:$GTPU"; do
    k=${pair%%:*}; v=${pair#*:}
    [ "$(sed -n "s/^${k}=//p" <<<"$oenv")" = "$v" ] && {
      echo "ABORT: $other is already running with $k=$v - it must be unique per UE" >&2
      exit 3; }
  done
done

# --keep: leave an already-attached UE alone. Recreating a UE that is carrying
# traffic tears down its PDU session, which is the last thing a multi-UE
# measurement wants.
if [ "$KEEP" = "--keep" ] && docker ps --format '{{.Names}}' | grep -qx "$NAME"; then
  docker logs "$NAME" > /tmp/_rg_keep.log 2>&1 || true
  if grep -q 'UE address' /tmp/_rg_keep.log; then
    echo "$NAME already attached, left alone (--keep)"
    exit 0
  fi
fi

docker rm -f "$NAME" >/dev/null 2>&1 || true
docker create --name "$NAME" --privileged \
  --network demo-oai-public-net --ip "$CPIP" \
  -e MCC=208 -e MNC=95 -e SST=222 -e SD=000005 -e DNN=default \
  -e NRCellID="$NRCELLID" -e GNBID="$GNBID" -e TAC=0x00a000 -e PagingDRX=v32 \
  -e MSIN="$MSIN" -e RoutingIndicator=1234 \
  -e KEY=0C0A34601D4F07677303652C0462535B \
  -e OPc=63bfa50ee6523365ff14c1f45f88737d \
  -e IMEISV=35609204079514 -e ProtectionScheme=null \
  `# DEREG_AFTER: gnbsim's entrypoint defaults this to 3600, and at exactly` \
  `# 3600 s after attach it DEREGISTERS the UE - the PDU session goes away,` \
  `# the UPF drops to "Sessions: 0", and anything still running fails with a` \
  `# broken pipe. It is silent: nothing warns, and the failure surfaces` \
  `# wherever the harness happened to be, an hour after the real cause.` \
  `# It cost repeats 2 and 3 of the first successful measurement run` \
  `# (PROJECT-HISTORY 22.3). A day is longer than any test session here.` \
  -e DEREG_AFTER="${DEREG_AFTER:-86400}" \
  -e RANUENGAPID="$RANUENGAPID" -e USE_FQDN=no \
  -e NGAPPeerAddr=192.168.70.132 \
  -e GTPuLocalAddr="$GTPU" -e GTPuIFname=eth1 \
  -e URL=http://www.asnt.org:8080/ \
  --entrypoint /gnbsim/bin/entrypoint.sh \
  gnbsim:latest sh -c '/gnbsim/bin/example && sleep infinity' >/dev/null
docker network connect --ip "$GTPU" oai-public-access "$NAME"
docker start "$NAME" >/dev/null
echo "$NAME recreated (MSIN=$MSIN GNBID=$GNBID RANUENGAPID=$RANUENGAPID NRCellID=$NRCELLID GTPu=$GTPU)"
