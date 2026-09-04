#!/bin/bash
# Reset the lab so UE1 -> internet-primary and UE2 -> internet-secondary.
# Waits on real conditions instead of fixed sleeps, and verifies as it goes.
set -u
PCF=192.168.70.139:8080
V=/openair-upf/bin/vppctl
# The collector image. Default is the build that BOUNDS qosmonlist - the older
# oai-nwdaf-sbi:final appends without limit, and a reset that quietly selects it
# undoes the fix: documents grow back to ~10 MB, analytics latency returns to
# 3-4 s and the SMF starts timing out with "NWDAF returned HTTP 0".
SBI_IMAGE=${SBI_IMAGE:-oai-nwdaf-sbi:qosmon-retain}
QOSMON_RETAIN=${QOSMON_RETAIN:-1000}

sess_count(){ sudo docker exec vpp-upf $V show upf session 2>/dev/null | grep -cE "^CP F-SEID"; }
# UE address for a given SEID, read from the UPF - authoritative and always
# present, unlike the gnbsim log which may not have flushed yet.
ue_addr(){ sudo docker exec vpp-upf $V show upf session 2>/dev/null \
  | awk -v want="$1" '/^CP F-SEID/{n=$0; sub(/.*\(/,"",n); sub(/\).*/,"",n)}
      n==want && /IPv4 address: 12/{sub(/\r$/,"",$3); print $3; exit}'; }
wait_for(){ local desc="$1" tries="$2"; shift 2
  for i in $(seq "$tries"); do eval "$@" >/dev/null 2>&1 && { echo "      ok: $desc"; return 0; }; sleep 2; done
  echo "      TIMEOUT waiting for: $desc"; return 1; }

echo "[1/8] stop traffic, remove SBI (must precede the SMF restart)"
sudo docker exec gnbsim-vpp2 pkill iperf3 2>/dev/null
sudo docker exec gnbsim-vpp3 pkill iperf3 2>/dev/null
sudo docker rm -f oai-nwdaf-sbi gnbsim-vpp2 gnbsim-vpp3 >/dev/null 2>&1

echo "[2/8] restart UPF (clears stale PFCP sessions) then SMF, then PCF"
sudo docker restart vpp-upf >/dev/null
wait_for "vpp-upf healthy" 30 '[ "$(sudo docker inspect -f "{{.State.Health.Status}}" vpp-upf)" = healthy ]'
sudo docker restart oai-smf >/dev/null
wait_for "oai-smf healthy" 30 '[ "$(sudo docker inspect -f "{{.State.Health.Status}}" oai-smf)" = healthy ]'
wait_for "PFCP association up" 30 'sudo docker exec vpp-upf '"$V"' show upf association | grep -q "Node: oai-smf"'
sudo docker restart oai-pcf >/dev/null
wait_for "oai-pcf healthy" 30 '[ "$(sudo docker inspect -f "{{.State.Health.Status}}" oai-pcf)" = healthy ]'
echo "      sessions now: $(sess_count) (expect 0)"

echo "[3/8] start SBI - AFTER the SMF is stable, BEFORE the UEs attach"
# Ordering matters twice over:
#  * it must come AFTER the SMF restart, or its subscription is duplicated;
#  * it must come BEFORE the UEs attach, or the UP_PATH_CH events that record
#    which DNAI each session is on are emitted with no subscriber and lost.
#    Losing them makes DN_PERFORMANCE attribute usage reports to a stale DNAI,
#    so the path a UE is really on reports "not provided" forever.
sudo docker create --name oai-nwdaf-sbi \
  --network demo-oai-public-net --ip 192.168.70.158 \
  -e SERVER_ADDR=0.0.0.0:8080 -e MONGODB_URI=mongodb://192.168.75.156:27017 \
  -e MONGODB_DATABASE_NAME=testing \
  -e MONGODB_COLLECTION_NAME_AMF=amf -e MONGODB_COLLECTION_NAME_SMF=smf \
  -e AMF_IP_ADDR=http://192.168.70.132:8080 -e AMF_SUBSCR_ROUTE=/namf-evts/v1 \
  -e AMF_API_ROUTE=/test/amf -e AMF_NOTIFICATION_ID=1 \
  -e AMF_NOTIFY_CORRELATION_ID=string -e AMF_NOTIFICATION_FORWARD_ROUTE=/sbi/notification/amf \
  -e AMF_HTTP_VERSION=2 \
  -e SMF_IP_ADDR=http://192.168.70.133:8080 -e SMF_SUBSCR_ROUTE=/nsmf-event-exposure/v1 \
  -e SMF_API_ROUTE=/test/smf -e SMF_NOTIFICATION_ID=2 \
  -e SMF_NOTIFY_CORRELATION_ID=string -e SMF_NOTIFICATION_FORWARD_ROUTE=/sbi/notification/smf \
  -e SMF_HTTP_VERSION=2 -e EVENT_NOTIFY_URI=http://oai-nwdaf-sbi:8080 \
  -e NRF_URI=http://192.168.70.130:8080 -e NRF_HTTP_VERSION=2 \
  -e MONGODB_QOSMON_RETAIN="$QOSMON_RETAIN" \
  "$SBI_IMAGE" >/dev/null
sudo docker network connect --ip 192.168.75.158 oai-nwdaf-net oai-nwdaf-sbi
sudo docker start oai-nwdaf-sbi >/dev/null; sleep 12


start_ue(){ # $1=name $2=ip70 $3=ip72 $4=msin $5=cell $6=gnbid $7=expected session count after
  echo "[$8/8] starting $1 (MSIN ...$4)"
  sudo docker create --name "$1" --privileged \
    --network demo-oai-public-net --ip "$2" \
    -e MCC=208 -e MNC=95 -e SST=222 -e SD=000005 -e DNN=default \
    -e NRCellID="$5" -e GNBID="$6" -e TAC=0x00a000 -e PagingDRX=v32 \
    -e MSIN="$4" -e RoutingIndicator=1234 \
    -e KEY=0C0A34601D4F07677303652C0462535B \
    -e OPc=63bfa50ee6523365ff14c1f45f88737d \
    -e IMEISV=35609204079514 -e ProtectionScheme=null \
    -e DEREG_AFTER=86400 -e RANUENGAPID=0 -e USE_FQDN=no \
    -e NGAPPeerAddr=192.168.70.132 \
    -e GTPuLocalAddr="$3" -e GTPuIFname=eth1 \
    -e URL=http://www.asnt.org:8080/ gnbsim:latest >/dev/null
  sudo docker network connect --ip "$3" oai-public-access "$1"
  sudo docker start "$1" >/dev/null
  wait_for "$1 attached (1 session)" 40 "[ \"\$(sudo docker exec vpp-upf $V show upf session 2>/dev/null | grep -cE '^CP F-SEID')\" = $7 ]"
}
start_ue gnbsim-vpp2 192.168.70.142 192.168.72.142 0000000032 1 5 1 4
start_ue gnbsim-vpp3 192.168.70.143 192.168.72.143 0000000033 2 6 2 5

UE1=$(ue_addr 1); UE2=$(ue_addr 2)

echo "[6/8] no AF app-session - the provisioned PCC rule already does the job"
# DELIBERATELY NO AF POST HERE. The provisioned rule 'steering-rule-both'
# already authorizes {access, internet-primary, internet-secondary} for UE1 at
# precedence 10, and lists internet-primary first, so UE1 starts on primary
# without any AF involvement.
#
# An AF app-session would install TrafficControlData 'af-tc-1' at PRECEDENCE 1,
# which OUTRANKS the provisioned rule - and the SMF selects only WITHIN the
# top-precedence rule. If that AF rule names one DNAI the SMF logs
#   "HOLD: only one PCF-authorized DNAI - no selection for analytics to make"
# and NWDAF-driven steering can never fire, with no way to delete the session
# (DELETE -> 404, POST .../delete -> 400).
#
# The POST that used to live here never worked anyway: $UE1 carried a trailing
# CR from vppctl, so the PCF rejected the JSON with HTTP 400. Removing it makes
# the demo purely NWDAF-driven - no AF anywhere in the loop.
wait_for "UE1 bound to primary" 20 "sudo docker exec vpp-upf $V show upf session 2>/dev/null | awk '/^CP F-SEID/{s=\$0} /internet.oai.org.pri/{if(s~/\\(1\\)/) found=1} END{exit !found}'"

ASID=$(sudo docker logs oai-pcf 2>&1 | grep -oP 'Location: .*/app-sessions/\K[0-9]+' | tail -1)
echo "[7/8] verifying DNAI timeline was captured"
sleep 15
echo "[8/8] done"
echo
echo "=================== RESULT ==================="
sudo docker exec vpp-upf $V show upf session 2>/dev/null | python3 -c '
import sys,re,subprocess
env=subprocess.run(["sudo","docker","inspect","vpp-upf","--format","{{range .Config.Env}}{{println .}}{{end}}"],capture_output=True,text=True).stdout
cfg=dict(l.split("=",1) for l in env.splitlines() if "=" in l)
m={cfg[f"IF_{i}_NWI"]:cfg[f"IF_{i}_DNAI"] for i in range(1,32) if cfg.get(f"IF_{i}_TYPE")=="N6"}
seid=core=nwi=None
for line in sys.stdin:
    s=line.strip()
    if s.startswith("CP F-SEID"): seid=re.search(r"\((\d+)\)",s).group(1); core=False; nwi=None
    elif s.startswith("PDR:"): core=False; nwi=None
    elif s.startswith("Source Interface: Core"): core=True
    elif core and s.startswith("Network Instance:") and nwi is None: nwi=s.split(":",1)[1].strip()
    elif core and nwi and s.startswith("IPv4 address:"):
        print("  SEID %-3s %-10s %s"%(seid,s.split(":",1)[1].strip(),m.get(nwi,"?"))); core=False; nwi=None'
SUBS=$(sudo docker logs oai-smf 2>&1 | awk '/\[start\] Started/{c=0} /Handle an Event Exposure Subscription Request/{c++} END{print c+0}')
echo "  sessions=$(sess_count) (expect 2)   subscriptions=$SUBS (expect 1)"
# STEERABILITY ASSERTION. Everything above can be green while the demo is still
# impossible: if the top-precedence PCC rule authorizes only ONE DNAI the SMF
# logs "HOLD: only one PCF-authorized DNAI" and no analytics-driven steer can
# ever fire. Catch that here rather than 5 minutes into a live demo.
# evaluate_sessions() logs the GOVERNING (lowest-numbered) precedence rule for
# each session, so whichever line mentions internet-primary is UE1's effective
# authorization - whatever precedence it happens to sit at.
# Match inside the "authorized {...}" SET only. The reason text of the OTHER
# session also contains the string "internet-primary" ("NWDAF-preferred DNAI
# 'internet-primary' is NOT authorized..."), so an unanchored grep picks the
# wrong line and reports the opposite of the truth.
DEC=$(sudo docker logs oai-smf --since 40s 2>&1 | grep 'per-session decision' \
      | grep -P 'authorized \{[^}]*internet-primary[^}]*\}' | tail -1)
if echo "$DEC" | grep -qP 'authorized \{[^}]*internet-primary[^}]*internet-secondary[^}]*\}'; then
  echo "  UE1 steerable: YES ($(echo "$DEC" | grep -oP 'precedence \d+'), both N6 DNAIs)"
else
  echo "  UE1 steerable: *** NO *** - analytics-driven steering CANNOT fire"
  echo "    ${DEC:-<no per-session decision logged yet - wait 20s and re-check>}"
fi
echo "=============================================="
echo "  export UE1=$UE1 UE2=$UE2 ASID=$ASID PCF=$PCF"
