#!/bin/bash
# Start iperf3 on every attached UE.  usage: demo_load_multi.sh [MBPS_EACH] [SECONDS]
#
# Aggregate rate matters: VPP forwards on ONE core here (vpp_main sits at 100%
# because it is poll-mode, not because it is loaded) and measured real capacity
# is ~683 Mbit/s. 5 UEs x 100 Mbit/s = 500 Mbit/s fits; 5 x 200 does not.
#
# The floor matters too: path health needs >=1000 tx packets per ~5 s interval
# (~2.4 Mbit/s per PATH) before it will report anything but
# UNKNOWN_INSUFFICIENT_SAMPLES. 100 Mbit/s per UE clears that comfortably.
#
# Each UE targets the DN leg on ITS OWN path. oai-ext-dn has one address per N6
# link (.73.135 on the primary side, .74.135 on the secondary side) and the map
# below is derived by matching subnets - nothing is hardcoded.
#
# WHY NOT ONE ADDRESS FOR EVERYONE. An earlier version pointed every UE at the
# primary leg on the theory that each N6 VRF has a default route to oai-ext-dn so
# either address works from either path. Measured: that holds for TCP but NOT for
# UDP from the secondary path - a UE whose session was ESTABLISHED on secondary
# connects to the primary leg and then transmits nothing, silently. It made the
# anchor UE look idle and left internet-secondary reporting UNKNOWN_NO_TRAFFIC
# while four other UEs hammered primary.
#
# A UE that is STEERED after its flow starts keeps running (observed: sessions
# migrated primary->secondary mid-test and secondary went on to report
# OBSERVED_HEALTHY on ~105k tx packets), so the target is chosen once, at start.
set -u
MBPS=${1:-100}
SECS=${2:-900}
PROTO=${3:-tcp}          # tcp | udp   - use udp for impairment tests
V=/openair-upf/bin/vppctl

# Build dnai -> DN address by matching each N6 interface's subnet against the
# addresses oai-ext-dn owns. Derived, so a re-addressed lab needs no edit here.
declare -A DN_OF_DNAI
while read -r dnai addr; do [ -n "$dnai" ] && DN_OF_DNAI[$dnai]=$addr; done < <(
python3 - <<'PYEOF'
import subprocess, ipaddress
def sh(c): return subprocess.run(c, capture_output=True, text=True).stdout
env = dict(l.split("=",1) for l in sh(["sudo","-n","docker","inspect","vpp-upf",
      "--format","{{range .Config.Env}}{{println .}}{{end}}"]).splitlines() if "=" in l)
# oai-ext-dn's addresses, one per N6 link
dn = []
for line in sh(["sudo","-n","docker","exec","oai-ext-dn","ip","-4","-o","addr","show"]).splitlines():
    f = line.split()
    if len(f) > 3 and "/" in f[3] and not f[3].startswith("127."):
        dn.append(ipaddress.ip_interface(f[3]))
# each N6 interface: DNAI + its own address, from the UPF container
ifaddr = {}
for line in sh(["sudo","-n","docker","exec","vpp-upf","ip","-4","-o","addr","show"]).splitlines():
    f = line.split()
    if len(f) > 3 and "/" in f[3]:
        ifaddr[f[1]] = ipaddress.ip_interface(f[3])
for i in range(1, 32):
    if env.get(f"IF_{i}_TYPE") != "N6":
        continue
    dnai = env.get(f"IF_{i}_DNAI")
    a = ifaddr.get(f"n6-{i}") or ifaddr.get(env.get(f"IF_{i}_NAME",""))
    if not (dnai and a):
        continue
    for d in dn:
        if d.network == a.network:
            print(dnai, d.ip); break
PYEOF
)
if [ ${#DN_OF_DNAI[@]} -eq 0 ]; then
  echo "could not derive the DNAI -> DN address map; is vpp-upf / oai-ext-dn up?" >&2
  exit 1
fi
echo "DN endpoint per DNAI:"
for k in "${!DN_OF_DNAI[@]}"; do echo "  $k -> ${DN_OF_DNAI[$k]}"; done

# UE IP -> DNAI it is currently on, read from the UPF (authoritative).
declare -A DNAI_OF_UE
while read -r ueip dnai; do [ -n "$ueip" ] && DNAI_OF_UE[$ueip]=$dnai; done < <(
  sudo docker exec vpp-upf $V show upf session 2>/dev/null | python3 -c '
import sys,re,subprocess
env=subprocess.run(["sudo","-n","docker","inspect","vpp-upf","--format",
  "{{range .Config.Env}}{{println .}}{{end}}"],capture_output=True,text=True).stdout
cfg=dict(l.split("=",1) for l in env.splitlines() if "=" in l)
m={cfg[f"IF_{i}_NWI"]:cfg[f"IF_{i}_DNAI"] for i in range(1,32) if cfg.get(f"IF_{i}_TYPE")=="N6"}
seid=None; ip={}; nwi={}
for line in sys.stdin:
    s=line.strip()
    mm=re.match(r"^CP F-SEID.*\((\d+)\)",s)
    if mm: seid=mm.group(1); continue
    if seid is None: continue
    if s.startswith("IPv4 address: 12.") and seid not in ip: ip[seid]=s.split(":",1)[1].strip()
    if s.startswith("Network Instance:"):
        v=s.split(":",1)[1].strip()
        if v in m and seid not in nwi: nwi[seid]=v
for k in ip:
    if k in nwi: print(ip[k], m[nwi[k]])')

# iperf3 -s serves ONE client per port at a time; a second client on the same
# port is refused. One port per UE.
echo "ensuring iperf3 servers on oai-ext-dn"
# Use iperf3's OWN daemon mode (-D). A backgrounded '&' inside `docker exec -d
# sh -c` is orphaned and killed the moment the shell exits, which silently left
# only the pre-existing servers listening and gave every later UE a dead client.
sudo docker exec oai-ext-dn pkill -f 'iperf3 -s' 2>/dev/null
sleep 1
for p in $(seq 5201 5215); do
  sudo docker exec oai-ext-dn iperf3 -s -D -p "$p" >/dev/null 2>&1
done
sleep 2
LISTEN=$(sudo docker exec oai-ext-dn sh -c 'ps aux | grep -c "[i]perf3 -s"')
echo "  $LISTEN iperf3 servers listening"

declare -A PORT_OF
n=0
for C in $(sudo docker ps --format '{{.Names}}' --filter 'name=gnbsim-vpp' | sort -V); do
  # Read the UE address from the container, not from a variable: demo_reset*.sh
  # only PRINTS its exports, and an empty -B makes iperf3 fail with
  # "unable to send control message: Bad file descriptor" - silently, under -d.
  UEIP=$(sudo docker exec "$C" ip -4 -o addr show 2>/dev/null | awk '/12\.1\.1\./{split($4,a,"/"); print a[1]; exit}')
  if [ -z "$UEIP" ]; then echo "  $C: NO UE ADDRESS - skipping"; continue; fi
  DNAI=${DNAI_OF_UE[$UEIP]:-}
  DN=${DN_OF_DNAI[$DNAI]:-}
  if [ -z "$DN" ]; then
    echo "  $C ($UEIP): no DNAI/DN mapping - is the session up? skipping"; continue
  fi
  n=$((n+1)); PORT=$((5200 + n))
  UDP=""; [ "$PROTO" = udp ] && UDP="-u"
  sudo docker exec -d "$C" iperf3 $UDP -c "$DN" -B "$UEIP" -p "$PORT" -t "$SECS" -b "${MBPS}M"
  printf "  %-14s %-10s on %-18s -> %s:%s @ %sM %s\n" \
    "$C" "$UEIP" "$DNAI" "$DN" "$PORT" "$MBPS" "$PROTO"
  PORT_OF[$C]=$PORT
  # Stagger. Launching clients back-to-back against just-restarted servers
  # wedges some of them: the process runs and burns CPU but moves ZERO bytes,
  # and the port then refuses new clients with "Connection reset by peer" until
  # that client is killed. Verified: same UE, same port, works immediately after
  # a restart of the client alone.
  sleep 2
done
sleep 4
echo
echo "verifying every load is actually MOVING BYTES (a dead client looks identical"
echo "to a healthy one if you only check that the process exists):"
declare -A B4
for C in $(sudo docker ps --format '{{.Names}}' --filter 'name=gnbsim-vpp' | sort -V); do
  B4[$C]=$(sudo docker exec "$C" sh -c "awk '/gtp-gnb/{print \$10}' /proc/net/dev" 2>/dev/null)
done
sleep 5
ok=0; bad=""
for C in $(sudo docker ps --format '{{.Names}}' --filter 'name=gnbsim-vpp' | sort -V); do
  NOW=$(sudo docker exec "$C" sh -c "awk '/gtp-gnb/{print \$10}' /proc/net/dev" 2>/dev/null)
  RATE=$(echo "(${NOW:-0}-${B4[$C]:-0})*8/5/1000000" | bc -l 2>/dev/null)
  if [ "$(echo "$RATE > 1" | bc -l 2>/dev/null)" = "1" ]; then
    printf "  %-14s %8.1f Mbit/s\n" "$C" "$RATE"; ok=$((ok+1))
  else
    printf "  %-14s %8.1f Mbit/s  *** NOT SENDING ***\n" "$C" "${RATE:-0}"; bad="$bad $C"
  fi
done
echo "  $ok/$n loads sending, aggregate offered ~$((ok*MBPS)) Mbit/s"
# Self-heal: restart just the wedged clients. Cheaper and more reliable than
# trying to prevent the race, and it reports honestly if a UE still will not send.
if [ -n "$bad" ]; then
  echo "  retrying wedged clients:$bad"
  for C in $bad; do
    sudo docker exec "$C" pkill iperf3 2>/dev/null
    UEIP=$(sudo docker exec "$C" ip -4 -o addr show 2>/dev/null | awk '/12\.1\.1\./{split($4,a,"/"); print a[1]; exit}')
    RDN=${DN_OF_DNAI[${DNAI_OF_UE[$UEIP]:-}]:-}
    sleep 1
    UDP=""; [ "$PROTO" = udp ] && UDP="-u"
    sudo docker exec -d "$C" iperf3 $UDP -c "$RDN" -B "$UEIP" -p "${PORT_OF[$C]}" -t "$SECS" -b "${MBPS}M"
    sleep 2
  done
  sleep 4
  echo "  after retry:"
  ok=0
  for C in $(sudo docker ps --format '{{.Names}}' --filter 'name=gnbsim-vpp' | sort -V); do
    A=$(sudo docker exec "$C" sh -c "awk '/gtp-gnb/{print \$10}' /proc/net/dev" 2>/dev/null)
    sleep 0  # measured over the 3 s below
    B=$A
    done_wait=1
  done
  sleep 3
  for C in $(sudo docker ps --format '{{.Names}}' --filter 'name=gnbsim-vpp' | sort -V); do
    NOW=$(sudo docker exec "$C" sh -c "awk '/gtp-gnb/{print \$10}' /proc/net/dev" 2>/dev/null)
    R=$(echo "(${NOW:-0}-${B4[$C]:-0})*8/1000000" | bc -l 2>/dev/null)
    if [ "$(echo "$R > 1" | bc -l 2>/dev/null)" = "1" ]; then
      printf "  %-14s SENDING\n" "$C"; ok=$((ok+1))
    else
      printf "  %-14s *** STILL NOT SENDING ***\n" "$C"
    fi
  done
  echo "  $ok/$n loads sending after retry"
fi
if [ $((ok*MBPS)) -gt 650 ]; then echo "  WARNING: above the ~683 Mbit/s single-core VPP ceiling"; fi
echo
echo "stop with: for c in \$(sudo docker ps --format '{{.Names}}' --filter name=gnbsim-vpp); do sudo docker exec \$c pkill iperf3; done"
