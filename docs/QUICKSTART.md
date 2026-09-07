# Quickstart

Goal: clone → build → run → attach a UE → generate traffic → see analytics → steer → verify.

## Prerequisites

| | |
|---|---|
| OS | Ubuntu 22.04 LTS (validated); anything with cgroup v2 and `iproute2` should work |
| Docker | 24+ (validated on 29.1.3), and your user in the `docker` group or `sudo` rights |
| docker-compose | **v1.29.2** — the compose file is v1 syntax. See the warning below |
| RAM / disk | ~8 GB RAM, ~40 GB free disk (the C++ NF build images are large) |
| Host packages | `iproute2` (`tc`), `python3`, `python3-venv`, `curl`, `bc`, `git` |
| Build | nothing else — every compiler runs inside a container |

```bash
sudo apt install -y iproute2 python3 python3-venv curl bc git docker-compose
```

**One external repository is required and is deliberately not vendored here:**
[`oai-cn5g-fed`](https://gitlab.eurecom.fr/oai/cn5g/oai-cn5g-fed) supplies
`database/oai_db2.sql` (the subscriber database — **without it no UE can
authenticate, and the failure looks like a RAN problem**) and
`healthscripts/`. `./scripts/build.sh fed` clones it at the pinned commit and
installs this repo's compose file, NF config and PCF policies into it.

> **docker-compose v1 warning.** On this host `docker-compose up` fails with
> `KeyError: 'ContainerConfig'` on locally-built images — **after** it has already killed
> the running container. Save `docker inspect <name> > backup.json` before any container
> change and recreate with `docker create`/`docker start`, replaying networks, env,
> entrypoint **and `HostConfig.PortBindings`**.

## 1. Build the images

**Nothing runs until this is done.** Nine images are needed and none of them are
published to a registry; four are built from this repository, three are OAI
network functions built from upstream source plus the patches in
[`patches/`](../patches/), one is a public image that is retagged, and one is
`mongo:latest`.

```bash
./scripts/build.sh              # everything
./scripts/build.sh nwdaf        # only the four Go services  (~2 minutes)
./scripts/build.sh fed          # only the oai-cn5g-fed tree + configs
./scripts/build.sh nfs          # only SMF, PCF, NRF — HOURS, see below
```

| Image | Built from | Time |
|---|---|---|
| `oai-nwdaf-engine:pathhealth` | `nwdaf/oai-nwdaf-engine` | seconds |
| `oai-nwdaf-nbi-analytics:pathhealth` | `nwdaf/oai-nwdaf-nbi-analytics` | seconds |
| `oai-nwdaf-nbi-events:final` | `nwdaf/oai-nwdaf-nbi-events` | seconds |
| `oai-nwdaf-sbi:qosmon-retain` | `nwdaf/oai-nwdaf-sbi` | seconds |
| `oai-smf:serialize` | upstream `667aa8fd` + `patches/smf/` | 30–120 min |
| `oai-pcf:heartbeat` | upstream `67aee53d` + `patches/pcf/` (**both** patches) | 30–120 min |
| `oai-nrf:nwdaf-disc-amfalias` | upstream `b38f13d8` + `patches/nrf/` (**both** patches) | 30–120 min |
| `gnbsim:latest` | retag of `rohankharade/gnbsim:latest` | seconds |
| AMF, UPF-VPP, UDR, UDM, AUSF, `mysql:8.0`, `mongo:latest` | pulled unmodified | — |

**The tags are load-bearing.** They encode which behaviour the image has, and
the deploy scripts check for them by name. Do not "tidy" them into `:latest`.

`build.sh` never re-patches a checkout that already has local modifications — it
builds it as it stands and says so, rather than silently discarding your work.

Env: `SRC_DIR` (upstream clones, default `$HOME/oai-src`), `FED_DIR` (default
`$HOME/oai-cn5g-fed`), and `SMF_TAG` / `PCF_TAG` / `NRF_TAG` to override a tag.

## 2. Apply the NF patches

`./scripts/build.sh nfs` does all of this for you. What follows is the same thing by
hand, for when you want to inspect or modify a patch first.

The OAI network functions are **not vendored**. Clone each upstream repo, check out the
pinned base commit named in the patch header, and apply:

```bash
git clone https://gitlab.eurecom.fr/oai/cn5g/oai-cn5g-smf.git
cd oai-cn5g-smf && git checkout 667aa8fd356d2fd569bf8aa1c610065e7be18f65
git apply /path/to/patches/smf/01-nwdaf-consumer-and-steering.patch
cp /path/to/patches/smf/smf_nwdaf_consumer.{cpp,hpp} src/smf_app/   # NEW files
```

Same for PCF (`67aee53d…`) and NRF (`b38f13d8…`).

**The PCF needs two patches.** `02-nrf-heartbeat.patch` is **not optional**: without
it the PCF stops sending NRF heartbeats, the NRF suspends and then deletes its
profile ~50 s after every start, and the SMF can no longer discover it. Nothing is
then authorized to steer — and the PCF still logs `NF registration successful`, so
the only symptom is that steering never happens.

**The NRF needs two patches.** `02-namf-communication-alias.patch` applies inside the
`src/common-src` **submodule**, not the NRF repo — apply it from
`oai-cn5g-nrf/src/common-src`. Without it the stock AMF cannot register (HTTP 400) and
leaks a socket per 20 s retry until its fd table saturates.

**AMF, UPF-VPP, UDR, UDM, AUSF are unmodified** — use the upstream images.

## 3. Deploy

Two ways, same scripts underneath. Use whichever you prefer — nothing only works
through `make`.

### With make

```bash
make            # list every target
make core       # 5G core + the steering SMF (asks which rule, see below)
make nwdaf      # NWDAF stack + host pollers
make ues        # attach UEs and the SBI, in the one order that works
```

or `make up` for all three.

### Or the scripts directly

```bash
cp compose/*.yaml                <oai-cn5g-fed>/docker-compose/
cp configs/nf/ulcl_config.yaml   <oai-cn5g-fed>/docker-compose/conf/
cp -r configs/pcf-policies/*     <oai-cn5g-fed>/docker-compose/policies/steering/

./scripts/deploy/start_core.sh              # or --rule HEALTH / --rule RATE
./scripts/deploy/start_nwdaf.sh
./scripts/lab/demo_reset_multi.sh 5 1
```

### Which steering rule?

`start_core.sh` asks, because the two behave completely differently and the
default is the weaker one:

| | ranks on | behaviour |
|---|---|---|
| **RATE** (default) | `avgTrafficRate` — **offered load** | steers *toward* the busier path; one-way, cannot return to an idle path |
| **HEALTH** | per-DNAI path health | steers only *away* from a path observed degraded; holds on healthy and on every UNKNOWN; reversible |

Both ranking rules are project-specific — TS 23.288 defines the analytic, not how
a consumer ranks DNAIs. What differs is the input: RATE reads a 3GPP field
(Table 6.14.3-1), HEALTH reads the vendor extension `oaiPathHealthExt`.

Pick `HEALTH` unless you are deliberately demonstrating the original behaviour.
See [MULTI-UE-STEERING.md](MULTI-UE-STEERING.md).

### Three things that will silently break this

**Image tags.** `nwdaf_stack_up.sh` defaults to `TAG=nwdaf-hardening`, which
predates the path-health work. An engine on that tag emits no `oaiPathHealthExt`,
so the HEALTH rule sees no health, every DNAI reads `UNKNOWN`, and **nothing ever
steers — with no error anywhere**. `start_nwdaf.sh` pins the right tags and prints
what it started; if you call `nwdaf_stack_up.sh` yourself, set `ENGINE_IMAGE`,
`ANALYTICS_IMAGE`, `EVENTS_IMAGE` explicitly.

**SBI ordering.** `oai-nwdaf-sbi` must start **after** the SMF is stable and
**before** any UE attaches. Too early and its subscription is duplicated, so every
usage report is counted twice; too late and the `UP_PATH_CH` events that record
which DNAI a session is on are emitted with no subscriber and lost — after which
`DN_PERFORMANCE` attributes usage to a stale DNAI forever. `demo_reset_multi.sh`
does this correctly; do not start the SBI by hand.

**Standalone containers block teardown.** `oai-smf` and the NWDAF containers run
outside compose, so `docker-compose down` skips them and then fails with
`network ... has active endpoints`. Remove them first — `make clean` does.

## 4. Verify registration

```bash
for T in AMF SMF UPF PCF NWDAF; do printf "%-6s " $T; \
  curl --http2-prior-knowledge -s \
  "http://192.168.70.130:8080/nnrf-nfm/v1/nf-instances?nf-type=$T" \
  | python3 -c "import sys,json;print(json.load(sys.stdin)['_links']['item'])"; done
```

AMF, SMF, UPF and NWDAF must all be present. If AMF is missing, patch 02 did not apply.

## 5. Telemetry and the DN route synchronizer

`start_nwdaf.sh` starts both and skips them if already running, so normally there
is nothing to do here. Confirm:

```bash
pgrep -af 'collect_upf_metrics.py|nwdaf_dn_route_sync.py'
```

To run them by hand (first time needs the venv):

```bash
python3 -m venv venv && venv/bin/pip install -r scripts/telemetry/requirements.txt
sudo setsid venv/bin/python3 scripts/telemetry/collect_upf_metrics.py --interval 5 &
sudo setsid python3 scripts/lab/nwdaf_dn_route_sync.py --interval 2 &
```

**Both are required, and both run outside any container.** The collector reads the
UPF container's cgroup and `vppctl`; without it there is no path health at all and
every DNAI reads `UNKNOWN`. The synchronizer rewrites `oai-ext-dn`'s return routes
so the DN answers on whichever N6 path a session was steered onto — without it a
steer breaks connectivity in both directions. See ARCHITECTURE.md.

## 6. Test it

```bash
make load                 # traffic on every UE  (UDP; see below)
make steer                # shape n6-3 so it starts stalling
make logs                 # watch the decision
make unsteer              # ALWAYS
make status               # where each session ended up
```

**Use UDP for impairment tests.** TCP's control connection collapses under a
sustained rate limit and the flow dies, which looks exactly like "no traffic" and
makes the path report `UNKNOWN` instead of `OBSERVED_DEGRADED`. `make load`
defaults to UDP.

**A leftover `tc` qdisc silently breaks every later test**, and `tc qdisc del` is
silent whether or not it removed anything — so `make unsteer` prints the resulting
state. Check it says `noqueue` on both interfaces.

What you should see within ~10 s of `make steer`:

```
NWDAF path health: dnai=internet-primary state=OBSERVED_DEGRADED stallPerPacket=0.0103
NWDAF per-session decision: PDU session 1 (precedence 10, authorized {access,
  internet-primary, internet-secondary}) -> SELECT 'internet-secondary'
Steering cycle: 4 eligible session(s), 1 steered (limit 1/cycle)
Steering: Update FAR 1 -> network instance 'internet.oai.org.sec'
```

`4 eligible session(s), 1 steered` is the serialization: sessions migrate one per
cycle, not all at once.

Full walkthrough, configuration reference and known limits:
[MULTI-UE-STEERING.md](MULTI-UE-STEERING.md).

## 7. Verify a checkout without touching a lab

```bash
make verify
```

Builds and tests every Go component, compiles the SMF C++, runs the unit tests,
checks the SMF patch still applies to the base commit it names, and scans for
credential material — entirely in temp directories and throwaway containers, so it
is safe to run while a demo is live.

## 8. Troubleshooting

Every entry below is a failure that has actually happened here, in the order you
are likely to meet them.

| Symptom | Cause | Fix |
|---|---|---|
| `missing image: oai-nwdaf-engine:pathhealth` | `build.sh` has not been run | `./scripts/build.sh nwdaf` |
| `oai-cn5g-fed not found at ...` | the external dependency is not cloned | `./scripts/build.sh fed`, or set `FED` |
| **Nothing ever steers, and there is no error anywhere** | the telemetry collector is not running, so every DNAI reads `UNKNOWN` | `pgrep -af collect_upf_metrics.py`; if empty, re-run `start_nwdaf.sh` and read `.collector.log` |
| Every DNAI reads `UNKNOWN` *with* the collector running | the NWDAF images are on a pre-`pathhealth` tag and emit no `oaiPathHealthExt` | check `docker inspect oai-nwdaf-engine --format '{{.Config.Image}}'` |
| `NWDAF returned HTTP 0` in the SMF | an SBI on a tag without `MONGODB_QOSMON_RETAIN`; documents grow to ~10 MB and the SMF times out | use `oai-nwdaf-sbi:qosmon-retain` |
| Steering never happens; the PCF logged `NF registration successful` | the PCF is missing `patches/pcf/02-nrf-heartbeat.patch`; the NRF deleted its profile ~50 s after start | rebuild the PCF with **both** patches; check `curl .../nf-instances?nf-type=PCF` |
| The AMF is absent from the NRF and its fd table saturates | `patches/nrf/02-namf-communication-alias.patch` was not applied **inside `src/common-src`** | it is a submodule — apply it from there |
| PFCP never associates; looks like a RAN failure | the SMF has no `--add-host` for the UPF FQDN | `recreate_smf.sh` sets it; do not start the SMF from compose |
| Every UE fails to authenticate | `database/oai_db2.sql` was not loaded into MySQL | it comes from `oai-cn5g-fed`; `./scripts/build.sh fed` |
| Control plane fine, 100 % packet loss, `traceroute` all `* * *` | `oai-ext-dn` bound its routes/NAT to the wrong interface — Docker does not guarantee eth0/eth1 order | `./scripts/deploy/fix_extdn.sh` |
| A steer "works" but the UE loses connectivity | the DN route synchronizer is not running | `pgrep -af nwdaf_dn_route_sync.py` |
| Usage-report rates are 2×, 3×… too high | the SBI was started more than once against a running SMF, duplicating its subscription | restart the SMF, then the SBI once — `demo_reset_multi.sh` does this in order |
| A test that passed yesterday now reports `UNKNOWN` | a `tc` qdisc was left behind | `make unsteer`; both interfaces must read `noqueue` |
| `KeyError: 'ContainerConfig'` and the container is already dead | docker-compose **v1** on a locally-built image | recreate by hand from a saved `docker inspect`, replaying networks, env, entrypoint **and `HostConfig.PortBindings`** |
| `docker-compose down` fails with `network ... has active endpoints` | `oai-smf` and the NWDAF containers run outside compose | remove them first — `make clean` does |

Two failure modes are worth stating separately because neither produces an error:

* **An idle path is byte-for-byte identical to a healthy one.** With no transmit
  attempt nothing can fail, so an impaired but idle path reads `UNKNOWN`, not
  `DEGRADED`. Start traffic *before* impairing anything.
* **Use UDP for impairment tests.** TCP's control connection collapses under a
  sustained rate limit and the flow dies, which looks exactly like "no traffic".

## 9. Tear down

```bash
make clean
```

Removes containers and networks but **not volumes**. The NWDAF MongoDB is on an
anonymous volume; `docker-compose down -v` would destroy every metric ever
collected. `mongodump` first if the history matters.
