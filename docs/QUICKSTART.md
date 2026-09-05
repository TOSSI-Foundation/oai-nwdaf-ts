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

## 3. Verify registration

```bash
for T in AMF SMF UPF PCF NWDAF; do printf "%-6s " $T; \
  curl --http2-prior-knowledge -s \
  "http://192.168.70.130:8080/nnrf-nfm/v1/nf-instances?nf-type=$T" \
  | python3 -c "import sys,json;print(json.load(sys.stdin)['_links']['item'])"; done
```

AMF, SMF, UPF and NWDAF must all be present. If AMF is missing, patch 02 did not apply.

## 4. Telemetry and the DN route synchronizer

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

## 5. Test it

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

## 6. Verify a checkout without touching a lab

```bash
make verify
```

Builds and tests every Go component, compiles the SMF C++, runs the unit tests,
checks the SMF patch still applies to the base commit it names, and scans for
credential material — entirely in temp directories and throwaway containers, so it
is safe to run while a demo is live.

## 7. Tear down

```bash
make clean
```

Removes containers and networks but **not volumes**. The NWDAF MongoDB is on an
anonymous volume; `docker-compose down -v` would destroy every metric ever
collected. `mongodump` first if the history matters.
