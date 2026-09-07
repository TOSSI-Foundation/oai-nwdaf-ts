# Shared readers for "where is each PDU session actually forwarding?".
# Sourced by scripts/test-*.sh. Not executable on its own.
#
# The UPF is the authority. The SMF's log says what it DECIDED; only the UPF's
# forwarding state says what actually happened, and Phase 13 of any honest test
# is the difference between those two.

V=/openair-upf/bin/vppctl

# seid -> "<ue-ip> <dnai>" for every session, read from the UPF's own session
# table and its own IF_n_NWI/IF_n_DNAI environment. Nothing is hardcoded, so a
# re-addressed lab needs no edit here.
upf_sessions(){
  sudo docker exec vpp-upf $V show upf session 2>/dev/null | python3 -c '
import sys, re, subprocess
env = subprocess.run(["sudo","-n","docker","inspect","vpp-upf","--format",
      "{{range .Config.Env}}{{println .}}{{end}}"], capture_output=True, text=True).stdout
cfg = dict(l.split("=",1) for l in env.splitlines() if "=" in l)
nwi2dnai = {cfg[f"IF_{i}_NWI"]: cfg[f"IF_{i}_DNAI"]
            for i in range(1,32) if cfg.get(f"IF_{i}_TYPE") == "N6"}
seid=None; ip={}; nwi={}; order=[]
for line in sys.stdin:
    s = line.strip()
    m = re.match(r"^CP F-SEID.*\((\d+)\)", s)
    if m:
        seid = m.group(1)
        if seid not in order: order.append(seid)
        continue
    if seid is None: continue
    if s.startswith("IPv4 address: 12.") and seid not in ip:
        ip[seid] = s.split(":",1)[1].strip()
    if s.startswith("Network Instance:"):
        v = s.split(":",1)[1].strip()
        if v in nwi2dnai and seid not in nwi: nwi[seid] = v
for k in order:
    if k in nwi:
        print(k, ip.get(k,"?"), nwi2dnai[nwi[k]])'
}

# How many sessions are on each DNAI, as "dnai count" lines.
upf_dnai_counts(){ upf_sessions | awk '{print $3}' | sort | uniq -c | awk '{print $2, $1}'; }

# Latest per-DNAI path health the collector wrote, as "dnai state ratio attempts".
# This is what the NWDAF turns into oaiPathHealthExt, so if this is empty the
# HEALTH rule cannot possibly fire and the test must say so rather than time out.
path_health(){
  sudo docker exec oai-nwdaf-database mongosh --quiet --eval '
    const c = db.getSiblingDB("testing").upf_metrics
              .find({dnaiPerf:{$exists:true}}).sort({timestamp:-1}).limit(1).toArray();
    if (!c.length) { print("NO-HEALTH-DATA"); quit(); }
    for (const [k,v] of Object.entries(c[0].dnaiPerf))
      print(k + " " + v.health.state + " " + v.health.sendtoFailurePerPacket
              + " " + v.health.txAttempts);' 2>/dev/null
}

# The N6 veth carrying a DNAI, derived from the UPF's IF_n_DNAI index. VPP names
# its host interfaces after the compose IF_n index, so this mapping is
# authoritative from the UPF's own environment - never guessed.
iface_of_dnai(){ # dnai -> n6-N
  sudo docker inspect vpp-upf --format '{{range .Config.Env}}{{println .}}{{end}}' 2>/dev/null \
  | awk -F= -v want="$1" '
      /^IF_[0-9]+_TYPE=N6$/   { split($1,a,"_"); n6[a[2]]=1 }
      /^IF_[0-9]+_DNAI=/      { split($1,a,"_"); d[a[2]]=$2 }
      END { for (i in n6) if (d[i]==want) { print "n6-" i; exit } }'
}
