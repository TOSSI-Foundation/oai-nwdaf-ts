# Quickstart

Goal: clone → build → run → attach a UE → generate traffic → see analytics → steer → verify.

## Prerequisites

Docker + docker-compose, ~8 GB RAM, `tc`/`iproute2` on the host, Go 1.20+ (or Docker) to
build the NWDAF, and a C++ toolchain (or Docker) for the OAI NFs.

> **docker-compose v1 warning.** On this host `docker-compose up` fails with
> `KeyError: 'ContainerConfig'` on locally-built images — **after** it has already killed
> the running container. Save `docker inspect <name> > backup.json` before any container
> change and recreate with `docker create`/`docker start`, replaying networks, env,
> entrypoint **and `HostConfig.PortBindings`**.

## 1. Apply the NF patches

The OAI network functions are **not vendored**. Clone each upstream repo, check out the
pinned base commit named in the patch header, and apply:

```bash
git clone https://gitlab.eurecom.fr/oai/cn5g/oai-cn5g-smf.git
cd oai-cn5g-smf && git checkout 667aa8fd356d2fd569bf8aa1c610065e7be18f65
git apply /path/to/patches/smf/01-nwdaf-consumer-and-steering.patch
cp /path/to/patches/smf/smf_nwdaf_consumer.{cpp,hpp} src/smf_app/   # NEW files
```

Same for PCF (`67aee53d…`) and NRF (`b38f13d8…`).

**The NRF needs two patches.** `02-namf-communication-alias.patch` applies inside the
`src/common-src` **submodule**, not the NRF repo — apply it from
`oai-cn5g-nrf/src/common-src`. Without it the stock AMF cannot register (HTTP 400) and
leaks a socket per 20 s retry until its fd table saturates.

**AMF, UPF-VPP, UDR, UDM, AUSF are unmodified** — use the upstream images.

## 2. Deploy

```bash
cp compose/*.yaml          <oai-cn5g-fed>/docker-compose/
cp configs/nf/ulcl_config.yaml   <oai-cn5g-fed>/docker-compose/conf/
cp -r configs/pcf-policies/*     <oai-cn5g-fed>/docker-compose/policies/steering/
docker-compose -f docker-compose-basic-vpp-pcf-steering.yaml up -d
./scripts/deploy/nwdaf_stack_up.sh
```

Order matters: `oai-nwdaf-sbi` starts **last and once**, and must be **removed before**
the SMF restarts, or it re-subscribes and every usage report is counted twice.

## 3. Verify registration

```bash
for T in AMF SMF UPF PCF NWDAF; do printf "%-6s " $T; \
  curl --http2-prior-knowledge -s \
  "http://192.168.70.130:8080/nnrf-nfm/v1/nf-instances?nf-type=$T" \
  | python3 -c "import sys,json;print(json.load(sys.stdin)['_links']['item'])"; done
```

AMF, SMF, UPF and NWDAF must all be present. If AMF is missing, patch 02 did not apply.

## 4. Start telemetry + the DN route synchronizer

```bash
python3 -m venv venv && venv/bin/pip install -r scripts/telemetry/requirements.txt
sudo setsid venv/bin/python3 scripts/telemetry/collect_upf_metrics.py --interval 5 &
sudo setsid python3 scripts/lab/nwdaf_dn_route_sync.py --interval 2 &
```

**Both are required.** Without the synchronizer, a steer breaks connectivity in both
directions — see ARCHITECTURE.md.

## 5. Attach a UE and generate traffic

```bash
./scripts/deploy/recreate_gnbsim.sh
UE=$(sudo docker exec vpp-upf /openair-upf/bin/vppctl show upf session \
     | grep -A1 'UE IP address (destination)' | grep -oE '12\.1\.1\.[0-9]+' | head -1)
sudo docker exec -d gnbsim-vpp2 iperf3 -c 192.168.73.135 -B $UE -p 5201 -t 300 -b 120M
```

## 6. Read the analytics

```bash
curl -s "http://127.0.0.1:6059/nnwdaf-analyticsinfo/v1/analytics?event-id=DN_PERFORMANCE" \
  | python3 -m json.tool
```

Each `dnPerf` entry carries the 3GPP `perfData` **and** `oaiPathHealthExt` with its state.

## 7. Steer, and verify

Authorize both DNAIs, then let the SMF act:

```bash
ASID=$(sudo docker logs oai-pcf 2>&1 | grep -oP 'app-sessions/\K[0-9]+' | tail -1)
curl -s --http2-prior-knowledge -X PATCH \
  "http://192.168.70.139:8080/npcf-policyauthorization/v1/app-sessions/$ASID" \
  -H 'Content-Type: application/merge-patch+json' \
  -d '{"ascReqData":{"afRoutReq":{"routeToLocs":[{"dnai":"internet-primary"},{"dnai":"internet-secondary"}]}}}'

sudo docker logs -f oai-smf | grep -E "SMF-initiated steering|Steering: Update"
```

Two different log lines can move a UE, and only one is the self-triggered loop:

```
✅ SMF-initiated steering: … no AF and no PCF notification involved   ← Track 2
❌ PCF-initiated modification: …                                      ← the PATCH did it
```

To see the first, **drain the 300 s window first** (stop traffic, pin the UE to one DNAI,
wait 5 min, re-authorize both, *then* apply load). Otherwise the re-authorization itself
triggers the move and the result is ambiguous.

Verify in the dataplane:

```bash
sudo docker exec vpp-upf /openair-upf/bin/vppctl show upf session | grep -E "F-SEID|Network Instance: internet"
sudo docker exec oai-ext-dn ip route | grep 12.1.1
sudo docker exec gnbsim-vpp2 ping -c 3 -I $UE 192.168.74.135
sudo docker logs --since 10m oai-smf | grep -c "Create SM Context Request"   # expect 0
```

Success is: network instance changed, DN route followed, 0% loss, **0 new PDU sessions**,
UE IP unchanged.
