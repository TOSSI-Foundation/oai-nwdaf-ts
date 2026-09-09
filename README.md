# NWDAF-Driven Traffic Steering on OAI 5G SA

*An NWDAF that measures each data-network path, and an SMF that acts on it — moving a live PDU session between two N6 paths without touching the UE.*

This repository extends the [OpenAirInterface 5G Core](https://gitlab.eurecom.fr/oai/cn5g)
so that the **NWDAF** measures per-path performance, the **PCF** authorizes a set of
exit points, and the **SMF** selects between them and reprograms the **UPF** — while
the subscriber's session stays up and keeps its IP address.

* * *

## Contents

| Section | |
|---|---|
| [Project Overview](#project-overview) | What this is and what it solves |
| [Architecture](#architecture) | Components and how data moves |
| [Traffic Steering](#traffic-steering) | The RATE and HEALTH modes |
| [Repository Layout](#repository-layout) | Directories and scripts |
| [Prerequisites](#prerequisites) | What you need before you start |
| [Clone the Repository](#clone-the-repository) | |
| [Verify the Repository](#verify-the-repository) | `make verify` |
| [Build](#build) | Images built locally vs pulled |
| [Deploy the 5G Core and NWDAF](#deploy-the-5g-core-and-nwdaf) | |
| [Prepare UEs and Test Traffic](#prepare-ues-and-test-traffic) | |
| [Run the HEALTH Steering Test](#run-the-health-steering-test) | |
| [Run the RATE Steering Test](#run-the-rate-steering-test) | |
| [Confirm Steering Actually Happened](#confirm-steering-actually-happened) | Proof, not exit codes |
| [Troubleshooting](#troubleshooting) | |
| [Clean Up and Reset](#clean-up-and-reset) | |
| [Limitations](#limitations) | Read before quoting results |
| [Development](#development) | Making changes |
| [Reproducibility](#reproducibility) | |
| [Quick Start](#quick-start) | If you already have the prerequisites |
| [Upstream and License](#upstream-and-license) | |

* * *

## Project Overview

### What this project is

A working 5G standalone lab in which the network **automatically moves a live data
session from one internet path to another**, based on measurements taken from the
user plane.

The subscriber does not re-attach, does not re-register, and keeps the same IP
address. Only the path the traffic takes on the far side of the core network
changes.

### The problem it solves

A 5G core normally chooses a data-network exit point when a session is created, and
then leaves it alone. If that path later becomes slow or unhealthy, nothing reacts.
Moving the session means tearing it down and building a new one, which the
subscriber notices.

This project closes that loop: the network measures each path continuously and moves
sessions between paths while they are running.

### What "traffic steering" means here

A subscriber's data session leaves the core network through the **UPF** on an
interface called **N6**. This lab gives the UPF **two** N6 paths to the data network:

| Path | DNAI (3GPP name) | UPF network instance | Linux interface | Subnet |
|---|---|---|---|---|
| Primary | `internet-primary` | `internet.oai.org.pri` | `n6-3` | `192.168.73.0/24` |
| Secondary | `internet-secondary` | `internet.oai.org.sec` | `n6-4` | `192.168.74.0/24` |

A **DNAI** (Data Network Access Identifier) is 3GPP's name for "which exit point the
traffic should use". **Traffic steering** in this project means *changing which DNAI
a live session uses*, without interrupting it.

### The rule that governs every decision

> **The NWDAF proposes. The PCF authorizes. The SMF decides. The UPF enforces.**

The NWDAF only ever publishes measurements — it never contacts the UPF and holds no
session state. The PCF publishes the set of DNAIs each subscriber is allowed to use.
The SMF may only choose *within* that set. This separation is deliberate and is the
reason the PCF is a required component rather than an optional one.

* * *

## Architecture


![System architecture: NWDAF-driven DNAI traffic steering on OAI 5G SA](docs/architecture.png)

*Figure A — Implemented system architecture. `MOD` marks an upstream OAI component
patched here, `NEW` a component written for this project, `LAB` test scaffolding.*


### Components

**Access and test harness**

| Component | What it is | Role |
|---|---|---|
| `gnbsim-vppN` | Container, `gnbsim:latest` | Simulated gNB **and** UE, one pair per container. UE addresses are allocated by the SMF from `12.1.1.0/26` (the `default` DNN pool in `configs/nf/ulcl_config.yaml`), starting at `12.1.1.2`. This is the only source of traffic. |
| `iperf3` | Runs inside the gnbsim containers | Generates the test load, aimed at the data network leg on that UE's own path. |
| `tc` token bucket | Kernel queueing discipline in the UPF's network namespace | The only way to make a path degrade in this lab. Nothing here can saturate a path on its own. |

**OAI 5G core** (control plane)

| Component | Image | Modified? | Role |
|---|---|---|---|
| `oai-amf` | `oaisoftwarealliance/oai-amf:v2.0.0` | No | Registration and mobility. |
| `oai-smf` | `oai-smf:serialize` | **Yes** | The only network function that decides and acts. Consumes NWDAF analytics, selects a DNAI within what the PCF authorized, and reprograms the UPF over PFCP. |
| `oai-pcf` | `oai-pcf:heartbeat` | **Yes** | Authorizes a set of DNAIs per subscriber. It does not choose between them. |
| `oai-nrf` | `oai-nrf:nwdaf-disc-amfalias` | **Yes** | Service discovery. Makes the NWDAF discoverable and fixes AMF registration. |
| `oai-ausf`, `oai-udm`, `oai-udr`, `mysql` | `v2.0.0` / `mysql:8.0` | No | Authentication and subscriber data. |

**User plane and data networks**

| Component | Image | Role |
|---|---|---|
| `vpp-upf` | `oaisoftwarealliance/oai-upf-vpp:v2.0.0` | Forwards traffic. Holds one forwarding table per network instance, so the two N6 paths are genuinely separate. Unmodified. |
| `oai-ext-dn` | `oaisoftwarealliance/trf-gen-cn5g:latest` | The external data network. Has one leg on each path: `192.168.73.135` on primary, `192.168.74.135` on secondary. Runs the `iperf3` servers. |

**NWDAF**

| Component | Image / form | Role |
|---|---|---|
| `oai-nwdaf-sbi` | `oai-nwdaf-sbi:qosmon-retain` | Subscribes to SMF and AMF event exposure and stores what it receives. This is how usage reports and DNAI changes reach the NWDAF. |
| UPF metrics collector | Host process, `scripts/telemetry/collect_upf_metrics.py` | Polls the UPF every 5 s for per-DNAI N6 interface counters, CPU and memory. **Runs outside any container.** |
| `oai-nwdaf-database` | `mongo:latest` | Stores usage reports, the DNAI timeline, and per-DNAI path telemetry. |
| `oai-nwdaf-engine` | `oai-nwdaf-engine:pathhealth` | Computes the analytics, including `DN_PERFORMANCE`. |
| `oai-nwdaf-nbi-analytics` | `oai-nwdaf-nbi-analytics:pathhealth` | Serves `Nnwdaf_AnalyticsInfo` on port 6059. This is what the SMF polls. Registers the NWDAF with the NRF. |
| `oai-nwdaf-nbi-events` | `oai-nwdaf-nbi-events:final` | Serves `Nnwdaf_EventsSubscription` on port 6060. Deployed, but nothing on the steering path consumes it. |

**Host processes** — two, and both are required:

| Process | Why it is required |
|---|---|
| `scripts/telemetry/collect_upf_metrics.py` | Without it there is **no path health at all**. Every DNAI reads `UNKNOWN` and the HEALTH rule can never fire. |
| `scripts/lab/nwdaf_dn_route_sync.py` | Keeps the external data network's per-UE return routes in step with the session's current path. Without it a steer breaks connectivity in **both** directions. |

### How information moves

```
1.  UE traffic                gnbsim -> N3/GTP-U -> vpp-upf -> N6 -> oai-ext-dn

2.  Measurement               vpp-upf usage reports --Nsmf_EventExposure--> oai-nwdaf-sbi
                              vpp-upf N6 counters   --host collector------> oai-nwdaf-database

3.  Analytics                 oai-nwdaf-engine computes DN_PERFORMANCE per DNAI,
                              plus a per-DNAI path-health extension

4.  Consumption               oai-smf --Nnwdaf_AnalyticsInfo (event-id=DN_PERFORMANCE)-->
                              oai-nwdaf-nbi-analytics :6059          (every 10 s)

5.  Authorization             oai-smf --N7--> oai-pcf returns the authorized DNAI set
                              (fetched at PDU session establishment)

6.  Decision                  oai-smf ranks the authorized DNAIs using the configured
                              rule (RATE or HEALTH) and selects one

7.  Actuation                 oai-smf --N4/PFCP--> vpp-upf
                              Update FAR (uplink egress) + Update PDR (downlink match)

8.  Result                    traffic now leaves on the other N6 network instance,
                              same session, same UE IP
```

Both PFCP rules live on the same N6 edge and **must move together**. Updating only
the FAR moves uplink egress while the downlink PDR still matches the old network
instance, and return traffic is silently dropped.

* * *

## Traffic Steering

The SMF supports two ranking rules. You choose one when you start the core, with
`make core RULE=RATE` or `make core RULE=HEALTH`. They are **mutually exclusive** —
the rule is fixed for the lifetime of the SMF container.

Both rules are project-specific. TS 23.288 defines the `DN_PERFORMANCE` analytic; it
does not define how a consumer ranks DNAIs.

### RATE-based steering

**Metric used.** `avgTrafficRate` from the `DN_PERFORMANCE` analytic — a 3GPP field
(TS 29.520 Table 6.14.3-1). It is averaged over `ENGINE_DN_PERFORMANCE_WINDOW_SEC`,
300 seconds by default.

**What the decision means.** `avgTrafficRate` is **offered load** — how many bits a
path is carrying. It is *not* a measure of path quality.

**How the preferred path is selected.** The SMF takes `argmax(avgTrafficRate)` over
the DNAIs the PCF authorized for that session. The busiest path wins.

**Consequences, stated plainly:**

* It steers **toward** the busier path, not away from a congested one.
* It is a **one-way ratchet**. To move a session from A to B, B must be busier. After
  the move, A carries less traffic and can never win it back.
* It is the original behaviour and the shipping default when
  `SMF_NWDAF_DNPERF_RULE` is unset.

**How the test demonstrates it.** Load the **anchor** UE, which is pinned to the
secondary path, and leave the steerable UEs on primary comparatively idle. Secondary
then reports the higher `avgTrafficRate`, and the primary sessions should migrate
onto it. No impairment is needed, so nothing has to be undone afterwards.

### HEALTH-based steering

**Health information used.** A per-DNAI path-health state, computed by the host
collector every 5 seconds from the UPF's N6 interface counters, and carried to the
SMF in a vendor extension called `oaiPathHealthExt` — deliberately outside the 3GPP
`PerfData` object.

The collector computes the state like this:

```
attempts = change in "tx packets"                       over the interval
failures = change in "tx sendto temporary failure"      over the interval

attempts == 0                                -> UNKNOWN_NO_TRAFFIC
attempts <  PATH_HEALTH_MIN_ATTEMPTS (1000)  -> UNKNOWN_INSUFFICIENT_SAMPLES
failures == 0                                -> OBSERVED_HEALTHY
failures  > 0                                -> OBSERVED_DEGRADED
```

**What happens when the current path becomes unhealthy.** The SMF looks at the health
of the DNAI the session is *actually* on, as confirmed by the UPF:

```
OBSERVED_DEGRADED  -> steer to the best authorized alternative
OBSERVED_HEALTHY   -> hold
UNKNOWN_*          -> hold        (unknown is NOT the same as bad)
```

Health older than `SMF_NWDAF_HEALTH_MAX_AGE_SEC` (30 s) is treated as `UNKNOWN`.

**How the alternative path is selected.** Among the authorized DNAIs other than the
current one, the SMF:

1. excludes any DNAI that is `OBSERVED_DEGRADED`;
2. excludes any DNAI seen degraded within `SMF_NWDAF_DEGRADED_MEMORY_SEC` (120 s) and
   not seen healthy since;
3. prefers `OBSERVED_HEALTHY` over `UNKNOWN_*`;
4. within a tier, picks the lowest `avgTrafficRate`.

Step 2 is what stops A→B→A ping-pong. After a session leaves a degraded path, that
path goes idle and reads `UNKNOWN_NO_TRAFFIC` — indistinguishable from healthy — so a
rule without memory would steer straight back.

**Serialization.** At most `SMF_NWDAF_STEER_MAX_PER_CYCLE` sessions (default 1) move
per evaluation cycle, so a degraded path drains gradually instead of all at once.

**What the health signal is, and is not.** `tx sendto temporary failure` is `sendto()`
returning `EAGAIN`/`ENOBUFS` at the AF_PACKET boundary between VPP and a Linux veth —
real egress backpressure. **It is not packet loss.** Under every impairment measured
in this lab, VPP's `drops` and Linux's `tx_dropped`/`tx_errors` all stayed at exactly
zero. It is a transmit-stall indicator specific to this VPP-on-veth setup, it is not
`avgPacketLossRate`, and it is not a 3GPP metric. See [Limitations](#limitations).

### Comparing the two

| | RATE | HEALTH |
|---|---|---|
| Input | `avgTrafficRate` (3GPP field) | `oaiPathHealthExt` (vendor extension) |
| Ranks on | Offered load | Path state |
| Direction | Toward the busier path | Away from a degraded path |
| Reversible | No — one-way ratchet | Yes |
| Detection window | 300 s average | 5 s interval |
| Needs impairment to demonstrate | No | Yes |
| Default | **Yes** | No |

Choose **HEALTH** unless you are specifically demonstrating the original RATE
behaviour.

* * *

## Repository Layout

```text
.
├── Makefile                 Thin wrapper over scripts/. Nothing works only via make.
├── README.md                This file - the only Markdown file in the repository.
├── .env.example             Every tunable, with its shipping default.
│
├── nwdaf/                   NWDAF services, written in Go
│   ├── oai-nwdaf-engine/         computes the analytics, including DN_PERFORMANCE
│   ├── oai-nwdaf-nbi-analytics/  serves Nnwdaf_AnalyticsInfo (what the SMF polls)
│   ├── oai-nwdaf-nbi-events/     serves Nnwdaf_EventsSubscription
│   └── oai-nwdaf-sbi/            collects SMF and AMF event exposure
│
├── patches/                 Changes to upstream OAI, pinned to base commits
│   ├── smf/                      NWDAF consumer, DNAI selection, PFCP actuation
│   ├── pcf/                      DNAI authorization + NRF heartbeat fix
│   ├── nrf/                      NWDAF discovery + AMF registration fix
│   └── deployment/               record of how this topology differs from upstream
│
├── compose/                 Deployment topology
├── configs/
│   ├── nf/ulcl_config.yaml       shared NF configuration
│   └── pcf-policies/             the PCF's authorization data
│
└── scripts/
    ├── build.sh                  builds every image this deployment needs
    ├── verify_release.sh         offline check of the checkout (touches no lab)
    ├── test-health.sh            automated HEALTH steering test, PASS/FAIL
    ├── test-rate.sh              automated RATE steering test, PASS/FAIL
    ├── deploy/
    │   ├── start_core.sh             5G core + the steering SMF
    │   ├── start_nwdaf.sh            NWDAF stack + the two host pollers
    │   ├── nwdaf_stack_up.sh         creates the NWDAF containers and network
    │   ├── recreate_smf.sh           replaces the SMF with the steering build
    │   ├── recreate_gnbsim.sh        recreates a single UE container
    │   └── fix_extdn.sh              repairs the data network's routes and NAT
    ├── lab/
    │   ├── demo_reset_multi.sh       generates PCF policy, restarts NFs, attaches UEs
    │   ├── demo_reset_2ue.sh         the older two-UE variant
    │   ├── demo_load_multi.sh        starts iperf3 on every attached UE
    │   └── nwdaf_dn_route_sync.py    keeps the DN return routes in step (host process)
    ├── telemetry/
    │   ├── collect_upf_metrics.py    per-DNAI UPF telemetry poller (host process)
    │   └── requirements.txt          pymongo, installed into .venv automatically
    └── lib/
        └── upf_state.sh              shared readers for "where is each session?"
                                      sourced, not executed
```

### The PCF policy files

`configs/pcf-policies/` is the PCF's authorization data. It has three parts, and all
three must be present:

| File | What it defines |
|---|---|
| `pcc_rules/pcc_rules.yaml` | Named rules, each referencing a traffic rule. `steering-rule-both` is the one that makes a session steerable. |
| `traffic_rules/traffic_rule.yaml` | The DNAI sets. `steering-scenario-both` lists `access`, `internet-primary` and `internet-secondary`. |
| `policy_decisions/policy_decision.yaml` | Maps each subscriber (SUPI) to a rule. **A SUPI with no entry here gets no DNAI authorization at all.** |

`make ues` regenerates `policy_decision.yaml` for however many UEs you ask for.

> The PCF loads **every** file in `policy_decisions/`. Never leave a backup copy in
> that directory — a stale file silently overrides the live one.

* * *

## Prerequisites

### Required

| Requirement | Detail |
|---|---|
| Operating system | Linux. Ubuntu 22.04 LTS is what this project is developed and validated on. |
| Docker Engine | Version 24 or newer. |
| Docker Compose | **A separate package from Docker.** Either the v2 plugin (`docker-compose-plugin`, recommended) or v1 (`docker-compose`). The scripts detect whichever is installed and refuse to start without one. |
| `sudo` rights | Every script calls `sudo docker`. |
| Passwordless `sudo` | The two host pollers are started with `sudo -n`, which fails rather than prompting. Without it they do not start, and **without the collector nothing ever steers**. |
| Host packages | `iproute2` (provides `tc`), `python3`, `python3-venv`, `curl`, `bc`, `git` |
| Network access | To clone from GitHub and GitLab, and to pull the unmodified OAI images from Docker Hub. |

```bash
sudo apt update
sudo apt install -y iproute2 python3 python3-venv curl bc git \
                    docker.io docker-compose-plugin
```

Notes on the less obvious ones:

* **Compose is not included in `docker.io`.** Installing Docker alone gives you
  neither `docker compose` nor `docker-compose`, and `make core` stops immediately.
* **`bc`** is used by `scripts/lab/demo_load_multi.sh` to check that each load is
  actually moving bytes.
* **Membership of the `docker` group is not required**, because the scripts use
  `sudo docker` throughout. It does no harm if you have it.
* **No Go or C++ toolchain is needed on the host.** Every compiler runs inside a
  container.
* **Python packages** — the only one needed is `pymongo`, and
  `scripts/deploy/start_nwdaf.sh` creates a virtual environment at `.venv/` and
  installs it for you on first run.

### External dependency

One external repository is required and is deliberately **not** vendored here:

**[`oai-cn5g-fed`](https://gitlab.eurecom.fr/oai/cn5g/oai-cn5g-fed)** supplies two
things this deployment cannot run without:

| File | Why it is required |
|---|---|
| `database/oai_db2.sql` | The subscriber database. Without it **no UE can authenticate**, and the failure looks like a radio problem. |
| `healthscripts/mysql-healthcheck2.sh` | Used by the MySQL container's health check. |

`make build-fed` clones it at a pinned commit and installs this repository's compose
file, NF configuration and PCF policies into it. It also becomes the working
directory for the deployment.

Default location: `$HOME/oai-cn5g-fed`. Override with the `FED_DIR` environment
variable at build time, or `FED` at deploy time.

### Optional

| Tool | Used for |
|---|---|
| `ss` (from `iproute2`) | Checking that the host ports below are free. |
| `mongosh` | Only ever run **inside** the `oai-nwdaf-database` container, so you do not need it on the host. |

### Host resources

This project does not enforce or check any hardware minimum, and none has been
measured. As a practical guideline from the environment it is developed on, allow
roughly **8 GB of RAM and 40 GB of free disk** — the OAI C++ build images are large.
Treat this as guidance, not a requirement.

### Host ports

These ports are published on the host and must be free:

| Port | Service |
|---|---|
| 27017 | MongoDB (`oai-nwdaf-database`) |
| 6059 | NWDAF analytics NBI — the one the SMF polls |
| 6060 | NWDAF events NBI |
| 6062 | NWDAF SBI |
| 6063 | NWDAF engine |

```bash
ss -lntp | grep -E ':(27017|6059|6060|6062|6063)\b' || echo "all free"
```

* * *

## Clone the Repository

```bash
git clone https://github.com/Mragankk/oai-nwdaf-traffic-steering.git
cd oai-nwdaf-traffic-steering
```

Every command in this guide is run from the root of this repository unless stated
otherwise.

Run `make` with no arguments at any time to list every available target.

* * *

## Verify the Repository

Before building anything, check that the checkout is complete and consistent:

```bash
make verify
```

This runs `scripts/verify_release.sh`. It works entirely in temporary directories
and throwaway containers: **no network function is restarted, no image is retagged,
and no database is written.** It is safe to run even while a deployment is live.

### What it checks

| Step | Check |
|---|---|
| 1 | All four NWDAF Go services compile and pass their unit tests |
| 2 | The SMF C++ additions compile and link |
| 3 | The SMF steering-serialization unit tests pass |
| 4 | The SMF patch still applies to the upstream commit it names |
| 5 | Every shell script, Python file and YAML file parses |
| 6 | Scripts have their executable bit and a shebang |
| 7 | Every other patch still applies to the upstream commit it names |
| 8 | No machine-specific absolute paths are hard-coded |
| 9 | The four NWDAF images build from this checkout |
| 10 | Every image tag the deploy scripts require is present locally |
| 11 | The external `oai-cn5g-fed` prerequisites are present |
| 12 | No private keys or access tokens are committed |

### Reading the result

| Result | Meaning |
|---|---|
| **PASS** | The check succeeded. |
| **FAIL** | Something is genuinely wrong. The run exits non-zero. Fix it before building. |
| **SKIP** | The check could not run because something optional is not on this machine — for example step 2 needs a cached SMF builder image that only exists after a full network-function build. **A SKIP is not a failure** and the run still exits zero. |
| **INFO** | Advisory. Step 10 reports images you have not built yet this way. |

On a fresh machine, expect several SKIPs. After a complete build, expect
`23 passed, 0 failed`.

### What it does not check

`make verify` proves the repository **builds**. It cannot prove that steering
**works** — that needs the UPF, SMF, PCF and real UEs, and is covered by
[the HEALTH test](#run-the-health-steering-test) and
[the RATE test](#run-the-rate-steering-test). Keep these two ideas separate:

* **Build verification** — `make verify`. Fast, offline, safe.
* **Runtime steering validation** — `make test-health`, `make test-rate`. Needs a
  live deployment.

* * *

## Build

Nothing is published to a container registry, so **you must build the images
yourself**. Nine images are needed in total.

### What is built and what is pulled

| Image | Source | Time |
|---|---|---|
| `oai-nwdaf-engine:pathhealth` | Built here from `nwdaf/oai-nwdaf-engine` | seconds |
| `oai-nwdaf-nbi-analytics:pathhealth` | Built here | seconds |
| `oai-nwdaf-nbi-events:final` | Built here | seconds |
| `oai-nwdaf-sbi:qosmon-retain` | Built here | seconds |
| `oai-smf:serialize` | Upstream `667aa8fd` + `patches/smf/` | **30–120 min** |
| `oai-pcf:heartbeat` | Upstream `67aee53d` + `patches/pcf/` (**both** patches) | **30–120 min** |
| `oai-nrf:nwdaf-disc-amfalias` | Upstream `b38f13d8` + `patches/nrf/` (**both** patches) | **30–120 min** |
| `gnbsim:latest` | Retag of the public `rohankharade/gnbsim:latest` | seconds |
| AMF, UPF-VPP, UDR, UDM, AUSF, `mysql:8.0`, `mongo:latest` | Pulled unmodified from Docker Hub | minutes |

> **The three OAI C++ network functions are the slow part.** Each is a full
> from-source build and takes between 30 minutes and 2 hours. Budget several hours
> for `make build-nfs` on a first run. The Go services, by contrast, build in about
> two minutes in total.

> **The image tags are load-bearing.** They record which behaviour an image has, and
> the deploy scripts look them up by name. Do not rename them to `:latest`.

### Build everything

```bash
make build
```

This runs the stages in dependency order: NWDAF services, gnbsim, the `oai-cn5g-fed`
tree, then the three C++ network functions.

### Build one part at a time

Recommended on a first run, so you can do the fast parts and check them before
committing several hours to the slow one.

```bash
make build-nwdaf      # the four NWDAF Go services            (~2 minutes)
make build-gnbsim     # the UE simulator                      (seconds)
make build-fed        # clone oai-cn5g-fed + install configs  (minutes)
make build-nfs        # SMF, PCF and NRF from source          (HOURS)
```

Each target is a single call to `scripts/build.sh`, which you can also run directly:

```bash
./scripts/build.sh nwdaf
./scripts/build.sh nfs
```

### Notes

* `build.sh` **never re-patches a checkout that already has local modifications.** It
  builds it as it stands and says so, rather than discarding your work silently.
* Upstream clones live in `$HOME/oai-src` by default. Override with `SRC_DIR`.
* Override an image tag with `SMF_TAG`, `PCF_TAG` or `NRF_TAG`.
* **The PCF needs both of its patches.** `02-nrf-heartbeat.patch` is not optional:
  without it the PCF stops sending NRF heartbeats, the NRF deletes its profile about
  50 seconds after every start, and the SMF can no longer discover it. The PCF still
  logs `NF registration successful`, so the only symptom is that steering never
  happens.
* **NRF patch 02 applies inside a submodule.** `build.sh` handles this. By hand,
  apply `02-namf-communication-alias.patch` from `oai-cn5g-nrf/src/common-src`, not
  from the repository root.

### Confirm the build

```bash
make verify
```

Step 10 should report `every required image tag is present`.

* * *

## Deploy the 5G Core and NWDAF

Start the system in three steps, in this order. Each step checks that the previous
one succeeded and refuses to continue if it did not.

```bash
make core RULE=HEALTH     # 1. the 5G core, including the PCF, plus the steering SMF
make nwdaf                # 2. the NWDAF stack and the two host pollers
make ues                  # 3. attach the UEs and the NWDAF SBI
```

`make up` runs all three in sequence.

If you omit `RULE=`, `make core` asks interactively which ranking rule to use.

### Step 1 — `make core`

Runs `scripts/deploy/start_core.sh`, which:

1. **Checks its prerequisites first** — that a compose implementation is installed,
   that `oai-cn5g-fed` is present with `oai_db2.sql`, that the three PCF policy
   directories exist, and that every locally-built image the compose file names is
   actually present.
2. **Starts the core with compose** — MySQL, NRF, AMF, AUSF, UDM, UDR, **the PCF**,
   the VPP UPF and the external data network. It then waits for each to report
   healthy.
3. **Checks the PCF specifically** — that its policy decisions are mounted inside the
   container, and that it is registered in the NRF. A PCF that is merely "healthy"
   can still have been purged from the NRF, which stops all steering with no error
   anywhere.
4. **Replaces the SMF** — the compose file ships the upstream SMF; the script removes
   it and recreates it standalone from `oai-smf:serialize` via
   `scripts/deploy/recreate_smf.sh`, with the NWDAF consumer environment set.
5. **Confirms the PFCP association** between the SMF and the UPF.

**About the PCF.** It is started by compose as part of `make core`. There is no
separate command for it and you should not start one by hand. Its configuration
comes from two mounts, both installed by `make build-fed`:

| Mount inside the container | Source in this repository |
|---|---|
| `/openair-pcf/etc/config.yaml` | `configs/nf/ulcl_config.yaml` |
| `/openair-pcf/policies` | `configs/pcf-policies/` |

`ulcl_config.yaml` sets `use_local_pcc_rules: no`, so the SMF fetches the SM policy —
and with it the authorized DNAI set — from the PCF at every PDU session
establishment. If the PCF is down, or has no `policy_decisions` entry for a
subscriber's SUPI, that session gets no alternative DNAI and the SMF logs
`HOLD: only one PCF-authorized DNAI` forever.

### Step 2 — `make nwdaf`

Runs `scripts/deploy/start_nwdaf.sh`, which starts the NWDAF containers on their own
network `192.168.75.0/24`, then starts the two required host pollers and **verifies
that they are actually running**.

Confirm at any time:

```bash
pgrep -af 'collect_upf_metrics.py|nwdaf_dn_route_sync.py'
```

Two things worth knowing:

* **Do not call `nwdaf_stack_up.sh` directly** unless you set `ENGINE_IMAGE`,
  `ANALYTICS_IMAGE`, `EVENTS_IMAGE` and `SBI_IMAGE`. Its own defaults point at an
  older image tag that emits no path health, after which nothing ever steers and
  there is no error to explain it. `start_nwdaf.sh` pins the correct tags.
* **The NWDAF network must stay on `192.168.75.0/24`.** The NWDAF compose files
  published by OAI use `192.168.74.0/24`, which is this deployment's **secondary N6
  network**. Override with `NWDAF_NET_PREFIX` if you genuinely need to move it.

### Step 3 — `make ues`

Runs `scripts/lab/demo_reset_multi.sh N_UES N_ANCHORS` (default `5 1`), which:

1. removes any existing gnbsim containers and the NWDAF SBI;
2. **generates the PCF policy decisions** for all N subscribers and writes them into
   the PCF's `policy_decisions/` directory;
3. restarts the UPF, then the SMF, then the **PCF**, waiting for each;
4. starts the NWDAF SBI — after the SMF is stable and before any UE attaches;
5. attaches the UEs one at a time;
6. prints where each session landed and how many are steerable.

```bash
make ues                    # 5 UEs: 4 steerable + 1 anchor
make ues UES=3 ANCHORS=1    # 3 UEs: 2 steerable + 1 anchor
```

**Steerable UEs and anchors.** The first `N_UES - N_ANCHORS` subscribers get the
`steering-rule-both` policy: they start on `internet-primary` and may be steered to
`internet-secondary` and back. The last `N_ANCHORS` get `steering-rule-secondary`:
they are pinned to secondary and cannot be steered.

The anchor exists to keep the secondary path **measurable**. Without at least one
anchor, the path no session is using only ever reads `UNKNOWN`, and neither rule has
two paths to compare.

**Why the SBI must start at exactly this point.** Too early and its subscription is
duplicated, so every usage report is counted twice and all rates read high. Too late
and the `UP_PATH_CH` events that record which DNAI a session is on are emitted with
no subscriber and lost, after which `DN_PERFORMANCE` attributes traffic to a stale
DNAI forever. `demo_reset_multi.sh` gets this right; do not start the SBI by hand.

### Confirm the deployment

Check that every network function registered with the NRF:

```bash
for T in AMF SMF UPF PCF NWDAF; do printf "%-6s " $T; \
  curl --http2-prior-knowledge -s \
  "http://192.168.70.130:8080/nnrf-nfm/v1/nf-instances?nf-type=$T" \
  | python3 -c "import sys,json;print(json.load(sys.stdin)['_links']['item'])"; done
```

**All five must be present.**

| Missing | Cause |
|---|---|
| **PCF** | Built without `patches/pcf/02-nrf-heartbeat.patch`; the NRF purged its profile. Nothing can be authorized to steer. |
| **AMF** | `patches/nrf/02-namf-communication-alias.patch` was not applied inside `src/common-src`. |
| **NWDAF** | The analytics NBI has not registered yet, or is not running. |

Then check the sessions and the steering rule in force:

```bash
make status
```

* * *

## Prepare UEs and Test Traffic

### UEs

`make ues` creates the UE containers. Each `gnbsim-vppN` container holds one
simulated gNB and one UE. With `make ues UES=3 ANCHORS=1` you get:

| Container | Role | Starts on |
|---|---|---|
| `gnbsim-vpp2` | steerable | `internet-primary` |
| `gnbsim-vpp3` | steerable | `internet-primary` |
| `gnbsim-vpp4` | **anchor** (last container) | `internet-secondary` |

The anchor is always the **highest-numbered** container.

### Test traffic

```bash
make load                              # defaults: MBPS=60 SECS=1800 PROTO=udp
make load MBPS=100 SECS=900 PROTO=udp
```

This runs `scripts/lab/demo_load_multi.sh`, which starts an `iperf3` client in every
attached UE container. Two details matter:

* **Each UE targets the data network leg on its own path.** `oai-ext-dn` has one
  address per N6 link and the script derives the mapping. A UE on the secondary path
  that sends to the primary leg transmits nothing over UDP, silently.
* **The script verifies that every load is actually moving bytes.** A wedged `iperf3`
  client looks identical to a healthy one if you only check that the process exists,
  so the script measures each UE and restarts the ones that are not sending.

**Use UDP for impairment tests.** TCP's control connection collapses under a
sustained rate limit and the flow dies, which looks exactly like "no traffic" and
makes the path report `UNKNOWN` instead of `OBSERVED_DEGRADED`. `make load` defaults
to UDP for this reason.

**Mind the throughput floor.** Path health needs at least 1000 transmitted packets
per five-second interval — roughly 2.4 Mbit/s per **path** — before it reports
anything other than `UNKNOWN_INSUFFICIENT_SAMPLES`. The default 60 Mbit/s per UE
clears this comfortably.

**Mind the ceiling too.** The VPP UPF forwards on a single core here, with a measured
capacity of about 683 Mbit/s. The script warns if the total offered load exceeds it.

To stop all traffic:

```bash
for c in $(sudo docker ps --format '{{.Names}}' --filter name=gnbsim-vpp); do
  sudo docker exec $c pkill iperf3
done
```

* * *

## Run the HEALTH Steering Test

This test proves that degrading one N6 path causes the SMF to move a live PDU session
onto the other one, and that the move is real in the UPF's forwarding state.

### Complete sequence from a clean environment

```bash
# 1. Bring the core up with the HEALTH rule.
make core RULE=HEALTH

# 2. Start the NWDAF stack and the host pollers.
make nwdaf

# 3. Attach UEs: 4 steerable on primary, 1 anchor pinned to secondary.
make ues UES=5 ANCHORS=1

# 4. Start UDP traffic on every UE and confirm it is moving bytes.
make load MBPS=60 SECS=1800 PROTO=udp

# 5. Run the test. It impairs a path, waits, and reports PASS or FAIL.
make test-health
```

Optional parameters: `make test-health WAIT=180 RATE=40mbit` (these are the
defaults). `WAIT` is how long to wait for a steer, in seconds.

### What the test does, step by step

**Preflight.** Checks that the UPF, SMF and the four NWDAF containers are running;
that the SMF's rule is actually `HEALTH`; that the telemetry collector is running;
and that at least two PDU sessions exist. It stops with a clear message rather than
timing out later.

**Baseline.** Records where every session is, how many sessions each DNAI carries,
and the current health of each path. It then chooses the path carrying the **most**
sessions as the one to impair — read from live state, so the test still works after a
previous run moved everything the other way.

It also checks that the chosen path is actually carrying traffic. A path with no
traffic cannot report `OBSERVED_DEGRADED`, because with no transmit attempt nothing
can fail.

**Impairment.** Applies a `tc` token-bucket qdisc to that path's N6 interface inside
the UPF's network namespace:

```
tc qdisc add dev n6-3 root tbf rate 40mbit burst 64kbit latency 400ms
```

The qdisc is removed on **every** exit path, including Ctrl-C.

**What the NWDAF observes.** Within about 5 seconds the collector sees
`tx sendto temporary failure` rising on that interface and publishes
`OBSERVED_DEGRADED` for that DNAI.

**What the SMF decides.** On its next 10-second poll the SMF reads the health of the
DNAI each session is on, finds it degraded, and selects the best authorized
alternative.

**What the SMF does.** It sends a PFCP Session Modification to the UPF — Update FAR
for uplink egress and Update PDR for the downlink match — moving the session to the
other network instance.

**How the move is confirmed.** The test re-reads the UPF's own session table and
compares it with the baseline, per SEID. The UPF is the authority; the log only says
what the SMF *decided*.

### What you should see

```
NWDAF path health: dnai=internet-primary state=OBSERVED_DEGRADED stallPerPacket=0.0103
NWDAF per-session decision: PDU session 1 (precedence 10, authorized {access,
  internet-primary, internet-secondary}) -> SELECT 'internet-secondary'
Steering cycle: 4 eligible session(s), 1 steered (limit 1/cycle)
Steering: Update FAR 1 -> network instance 'internet.oai.org.sec'
```

`4 eligible, 1 steered` is serialization working as designed.

### What indicates PASS

The test prints four assertions and a verdict. **PASS requires all of them:**

| Assertion | Meaning |
|---|---|
| `detection` | The impaired DNAI reported `OBSERVED_DEGRADED` |
| `decision` | The SMF logged a selection of the expected alternative DNAI |
| `actuation` | At least one session changed network instance **in the UPF** |
| `serialization` | No more than one steer occurred in any single cycle |

The final line is:

```
RESULT: PASS - HEALTH steering detected, decided and actuated
```

and the script exits zero. Anything else is a failure, and the failing assertion is
named.

### Afterwards

The test removes its own impairment. Confirm nothing was left behind:

```bash
make unsteer
```

Both interfaces must read `noqueue`. **A leftover qdisc silently breaks every later
test**, and `tc qdisc del` is silent whether or not it removed anything, which is why
`make unsteer` prints the resulting state.

### Doing it manually

If you want to watch it happen rather than get a verdict:

```bash
make load
make logs        # in one terminal - follow the SMF's decisions
make steer       # in another - impair n6-3 at 40mbit
make status      # see where the sessions are now
make unsteer     # ALWAYS
```

* * *

## Run the RATE Steering Test

This test proves that the SMF ranks DNAIs by `avgTrafficRate` and moves sessions
**toward** the busier path. That is the documented RATE behaviour, and a rule that
quietly stopped doing it would be a regression.

### The traffic shape RATE needs

RATE compares offered load, so the test needs a **rate difference** between the two
paths, with at least one session sitting on the slower one:

* the **anchor** UE, pinned to secondary, carries load;
* the **steerable** UEs, on primary, stay comparatively idle.

Secondary then reports the higher `avgTrafficRate`, and the primary sessions should
migrate onto it. No impairment is involved, so nothing has to be undone.

### Complete sequence from a clean environment

```bash
# 1. Bring the core up with the RATE rule.
#    SMF_NWDAF_PREDICT_SEC=0 selects statistics instead of predictions - see below.
SMF_NWDAF_PREDICT_SEC=0 make core RULE=RATE

# 2. Start the NWDAF stack and the host pollers.
make nwdaf

# 3. A small topology is easiest to reason about: 2 steerable + 1 anchor.
make ues UES=3 ANCHORS=1

# 4. Start traffic on every UE, then stop it on the steerable ones so that only
#    the anchor (the highest-numbered container) keeps sending.
make load MBPS=60 SECS=1800 PROTO=udp
for c in gnbsim-vpp2 gnbsim-vpp3; do sudo docker exec $c pkill iperf3; done

# 5. Let the rate difference build, then run the test.
make test-rate
```

Optional parameter: `make test-rate WAIT=300` (the default when run through `make`).

> **Why `SMF_NWDAF_PREDICT_SEC=0`.** With the default of 60, the SMF asks the NWDAF
> for *predictions*, which carry a Confidence value, and it discards any DNAI whose
> Confidence is below `SMF_NWDAF_MIN_CONFIDENCE` (50). Confidence rises only as the
> 300-second analytics window fills — measured at 39 one minute after traffic starts
> and crossing 50 at around five minutes — so for the first few minutes **every** DNAI
> is rejected and nothing is ranked. Setting it to `0` selects *statistics*, which
> carry no Confidence at all, so the floor never applies. This is the reliable way to
> exercise RATE. If you leave the default, simply keep the traffic running longer.

> **Be patient.** The engine averages `DN_PERFORMANCE` over 300 seconds, so the rate
> separation has to build up before there is anything to rank. This is why the RATE
> test waits longer than the HEALTH one by default.

### What the test does, step by step

**Preflight.** Checks the same containers as the HEALTH test, confirms the SMF's rule
is `RATE`, and requires at least two PDU sessions.

**Baseline.** Records where every session is and how many each DNAI carries. If every
session is already on one path, the test stops immediately and says so — RATE is a
one-way ratchet, and once everything has converged there is no second path to compare.

**What the NWDAF reports.** The test queries the analytics NBI directly — the same
interface the SMF consumes, not the database behind it:

```bash
curl -s "http://127.0.0.1:6059/nnwdaf-analyticsinfo/v1/analytics?event-id=DN_PERFORMANCE"
```

It parses `dnPerfInfos[].dnPerf[].perfData.avgTrafficRate` for each DNAI, prints the
rates, and names `argmax(avgTrafficRate)` as the DNAI sessions should converge on.

If the SMF is currently rejecting DNAIs on the confidence floor, the test says so
explicitly rather than letting you guess.

**What the SMF decides.** On each 10-second poll it ranks the authorized DNAIs by
`avgTrafficRate` and selects the highest.

**How the move is confirmed.** As with HEALTH, by re-reading the UPF's session table
and comparing per SEID against the baseline — and additionally by checking the
**direction** of the move.

### What indicates PASS

| Assertion | Meaning |
|---|---|
| `decision` | The SMF logged a per-session selection |
| `actuation` | At least one session changed network instance in the UPF |
| `direction` | The session moved **toward** the highest-rate DNAI |

The final line is:

```
RESULT: PASS - RATE steering decided and actuated
```

and the script exits zero.

The `direction` assertion is what makes this a real test of RATE rather than a test
that "something moved". Moving *away* from the busiest DNAI is reported as a failure.

### Running both tests

The two rules **cannot be tested back to back.** `SMF_NWDAF_DNPERF_RULE` is fixed on
the SMF container, so each test needs its own `make core`, and RATE additionally
needs a different traffic shape. Running `make test` prints exactly this and exits
non-zero rather than pretending otherwise.

To run both, do a full cycle for each:

```bash
make core RULE=HEALTH && make nwdaf && make ues && make load
make test-health
make unsteer

SMF_NWDAF_PREDICT_SEC=0 make core RULE=RATE && make nwdaf && make ues UES=3 ANCHORS=1
make load
for c in gnbsim-vpp2 gnbsim-vpp3; do sudo docker exec $c pkill iperf3; done
make test-rate
```

* * *

## Confirm Steering Actually Happened

**Do not rely on a script's exit code alone.** A test can pass for the wrong reason,
and a log line only says what the SMF *decided*. The UPF's forwarding state is the
only authority on what actually happened.

Check these four things, in this order.

### 1. Where the sessions are, according to the UPF

```bash
make status
```

This reads the UPF's own session table and prints one line per session — the SEID,
the UE's IP address, and the network instance it is forwarding on — followed by the
current per-DNAI health and the ranking rule in force.

Compare the network instance before and after the steer. `internet.oai.org.pri` is
the primary path; `internet.oai.org.sec` is the secondary.

For the raw table:

```bash
sudo docker exec vpp-upf /openair-upf/bin/vppctl show upf session
```

> `vppctl` is at `/openair-upf/bin/vppctl` and is **not on `$PATH`**. Plain
> `docker exec vpp-upf vppctl …` fails.

### 2. That the UE's IP address did not change

This is the central claim of the project. In the `make status` output, the UE address
column must be **identical** before and after the steer for the same SEID. The
session did not restart; only its exit path changed.

If the IP changed, the session was re-established rather than steered, and the result
does not demonstrate what it appears to.

### 3. What the SMF decided, and why

```bash
make logs
```

`make logs` is a shortcut. To read the SMF log directly — useful in a second
terminal, or when you want to change the filter — run the command it wraps:

```bash
sudo docker logs -f oai-smf 2>&1 | grep --line-buffered -E \
  "Steering cycle:|per-session decision|Steering: Update|dnai="
```

`--line-buffered` matters: without it `grep` buffers its output and the lines
arrive in bursts long after the event, which makes a live steer impossible to
follow. Drop the pipe entirely (`sudo docker logs -f oai-smf`) to see everything.

For the log so far rather than a live follow, replace `-f` with `--since`:

```bash
sudo docker logs --since 5m oai-smf 2>&1 | grep -E \
  "Steering cycle:|per-session decision|Steering: Update|dnai="
```

Either way, you are looking for:

| Log line | What it tells you |
|---|---|
| `dnai=… state=…` | The path health the SMF received from the NWDAF |
| `per-session decision … -> SELECT '<dnai>'` | The DNAI it chose, and the set the PCF authorized |
| `Steering cycle: N eligible session(s), M steered` | How many were eligible and how many actually moved |
| `Steering: Update FAR … -> network instance '…'` | The PFCP change it pushed to the UPF |

A `HOLD` line means the SMF evaluated the session and decided not to move it. The
reason is printed after the arrow. The two you are most likely to see:

| `HOLD` reason | What it means |
|---|---|
| `only one PCF-authorized DNAI (…)` | The PCF authorized a single path for that subscriber, so there is nothing to choose between. A policy problem, not a steering problem — check `policy_decisions/` and re-run `make ues`. |
| `the UPF has not confirmed a DNAI for this session yet` | The SMF knows about the session but the UPF has not yet reported which DNAI it is forwarding on. Normal for a few seconds after a session starts. If it persists, the SMF and UPF have drifted apart — `make ues` resets both. |

A steady stream of `HOLD` with healthy or `UNKNOWN` paths is the correct, safe
behaviour, not a fault: the HEALTH rule only acts on a path it has observed to be
degraded.

### 4. What the NWDAF actually measured

The per-DNAI health the collector wrote most recently:

```bash
sudo docker exec oai-nwdaf-database mongosh --quiet --eval '
 const d = db.getSiblingDB("testing").upf_metrics
             .find({dnaiPerf:{$exists:true}}).sort({timestamp:-1}).limit(1).toArray()[0];
 for (const [k,v] of Object.entries(d.dnaiPerf))
   print(k + "  " + v.health.state + "  ratio=" + v.health.sendtoFailurePerPacket
           + "  n=" + v.health.txAttempts)'
```

And the analytic as the SMF sees it:

```bash
curl -s "http://127.0.0.1:6059/nnwdaf-analyticsinfo/v1/analytics?event-id=DN_PERFORMANCE"
```

### A steer is genuine when all of these hold

* the network instance for a given SEID changed in the UPF;
* the UE's IP address for that SEID did **not** change;
* the SMF logged a `SELECT` and an `Update FAR` for it;
* the NWDAF had published a state or rate that justifies the choice.

* * *

## Troubleshooting

Every entry below is a failure that has actually occurred in this project.

### Environment and permissions

| Symptom | Likely cause | Fix |
|---|---|---|
| `sudo: docker-compose: command not found` | Neither compose generation is installed; `docker.io` does not include one | `sudo apt install -y docker-compose-plugin` |
| Permission denied talking to the Docker socket | The commands need `sudo` | Run with `sudo` rights; the scripts already call `sudo docker` |
| The host pollers never start; `pgrep` finds nothing | `sudo -n` failed because sudo wants a password | Configure passwordless `sudo`, then re-run `make nwdaf` and read `.collector.log` |
| `KeyError: 'ContainerConfig'` and the container is already dead | docker-compose **v1** on a locally-built image — it kills the container before failing | Use the v2 plugin |
| `dial tcp [2600:9000:…]:443: network is unreachable` while pulling | DNS returned an IPv6 address for Docker Hub and the host has no IPv6 route | Retry; if it persists, `sudo sysctl -w net.ipv6.conf.all.disable_ipv6=1 && sudo systemctl restart docker` |

### Build and images

| Symptom | Likely cause | Fix |
|---|---|---|
| `pull access denied for oai-smf` (or any `oai-*`) | The compose file names a locally-built tag that was never built | `make build-nfs` |
| `missing image: oai-nwdaf-engine:pathhealth` | The Go services have not been built | `make build-nwdaf` |
| `oai-cn5g-fed not found at …` | The external dependency is not cloned | `make build-fed`, or set `FED` |
| `make verify` reports a patch does not apply | Upstream moved, or the local clone is dirty | Check `$HOME/oai-src/<repo>`; `build.sh` will not re-patch a dirty tree |

### Deployment

| Symptom | Likely cause | Fix |
|---|---|---|
| `FAIL: PCF policy directory missing: …` | The PCF policies were never installed into the fed tree | `make build-fed` |
| `network demo-oai-public-net not found` during `make nwdaf` | The core never came up | Fix `make core` first, then re-run |
| `No such container: vpp-upf` during `make ues` | The core is not running | `make core` first |
| Every UE fails to authenticate | `oai_db2.sql` was not loaded into MySQL | It comes from `oai-cn5g-fed`; `make build-fed` |
| PFCP never associates; looks like a radio failure | The SMF has no `--add-host` for the UPF's FQDN | `recreate_smf.sh` sets it — do not start the SMF from compose |
| Control plane fine, 100 % packet loss, `traceroute` shows `* * *` | `oai-ext-dn` bound its routes to the wrong interface; Docker does not guarantee interface order | `./scripts/deploy/fix_extdn.sh` |
| `docker compose down` fails with `network … has active endpoints` | The SMF and the NWDAF containers run outside compose | Remove them first — `make clean` does |
| A container name conflict on a second `make core` | A container was created outside compose | `sudo docker rm -f <name>` and re-run |

### Registration

| Symptom | Likely cause | Fix |
|---|---|---|
| **PCF absent from the NRF**, but it logged `NF registration successful` | Built without `patches/pcf/02-nrf-heartbeat.patch`; the NRF purged its profile ~50 s after start | Rebuild the PCF with **both** patches |
| AMF absent from the NRF, and its file-descriptor table fills up | `patches/nrf/02-namf-communication-alias.patch` was not applied **inside `src/common-src`** | It is a submodule — apply it from there |
| NWDAF absent from the NRF | The analytics NBI is not running, or registration has not completed | Check `oai-nwdaf-nbi-analytics` logs; `start_nwdaf.sh` polls for 60 s |

### Steering does not happen

| Symptom | Likely cause | Fix |
|---|---|---|
| **Nothing steers and there is no error anywhere** | The telemetry collector is not running, so every DNAI reads `UNKNOWN` | `pgrep -af collect_upf_metrics.py`; if empty, re-run `make nwdaf` and read `.collector.log` |
| Every DNAI reads `UNKNOWN` *with* the collector running | The NWDAF images are on a pre-`pathhealth` tag and emit no health | `sudo docker inspect oai-nwdaf-engine --format '{{.Config.Image}}'` |
| SMF logs `HOLD: only one PCF-authorized DNAI` | That SUPI has no `steering-rule-both` entry | `make ues` regenerates it; check for a stale backup left in `policy_decisions/` |
| `NWDAF returned HTTP 0` in the SMF | An SBI without `MONGODB_QOSMON_RETAIN`; documents grow to ~10 MB and the SMF times out | Use `oai-nwdaf-sbi:qosmon-retain` |
| RATE test does nothing; SMF logs `below the configured confidence floor of 50` | Predictions are rejected until Confidence rises | Keep traffic running, or use `SMF_NWDAF_PREDICT_SEC=0` |
| RATE test reports `all N sessions are already on <dnai>` | RATE is a one-way ratchet and everything has converged | `make ues UES=3 ANCHORS=1`, then load the anchor only |
| A path reads `UNKNOWN` although it is impaired | It is idle — an idle impaired path is byte-for-byte identical to an idle healthy one | Start traffic **before** impairing anything |
| A test that passed yesterday now reports `UNKNOWN` | A `tc` qdisc was left behind | `make unsteer`; both interfaces must read `noqueue` |
| Usage-report rates are 2× or 3× too high | The SBI was started more than once against a running SMF, duplicating its subscription | Restart the SMF, then the SBI once — `make ues` does this in order |
| A steer "works" but the UE loses connectivity | The DN route synchronizer is not running | `pgrep -af nwdaf_dn_route_sync.py` |

* * *

## Clean Up and Reset

These are listed from least to most destructive. Read the warning on each.

### Stop test traffic only

Leaves everything deployed and every session in place.

```bash
for c in $(sudo docker ps --format '{{.Names}}' --filter name=gnbsim-vpp); do
  sudo docker exec $c pkill iperf3
done
```

### Remove path impairment

Always run this after any test that shaped an interface.

```bash
make unsteer
```

Both `n6-3` and `n6-4` must report `noqueue`.

### Reset the UEs and start over

⚠️ **This removes and recreates all UE containers, restarts the UPF, SMF and PCF, and
rewrites the PCF's policy decisions.** Existing PDU sessions are destroyed. The core
itself stays up.

```bash
make ues UES=5 ANCHORS=1
```

Use this between test runs. Wait until both paths report `0 bps` before resetting —
the engine averages over 300 seconds and MongoDB is not restarted, so an immediate
reset makes decisions on stale data.

### Restart the core with a different rule

⚠️ **This recreates the core containers and the SMF.** All sessions are lost and the
UEs must be re-attached afterwards.

```bash
make core RULE=RATE
make ues
```

### Rebuild images

Safe to run at any time. Rebuilding a tag does not affect an already-running
container; the new image is used the next time the container is created.

```bash
make build-nwdaf
```

### Tear down completely

⚠️ **This stops and removes every container and network in the deployment.**

```bash
make clean
```

It removes containers and networks but **deliberately not volumes**. The NWDAF
MongoDB is on an anonymous volume, and `docker compose down -v` would destroy every
metric ever collected. Run `mongodump` first if the history matters to you.

Standalone containers are removed first: the SMF and the NWDAF containers run outside
compose, so compose does not know about them, and their network endpoints would
otherwise block network removal.

After `make clean`, start again from [Deploy](#deploy-the-5g-core-and-nwdaf). The
images do not need rebuilding.

* * *

## Limitations

These are current, real limitations. Read this section before quoting any result.

### The path-health metric is not packet loss

`sendtoFailurePerPacket` is the change in `tx sendto temporary failure` divided by
the change in `tx packets`, measured at the AF_PACKET boundary between VPP and a
Linux veth. It counts transmit **stalls** — the kernel returning `EAGAIN`/`ENOBUFS` —
not discarded packets.

Under every impairment measured, VPP's interface `drops` and Linux's `tx_dropped` and
`tx_errors` all stayed at exactly **zero**.

It is **not** `avgPacketLossRate`, **not** any 3GPP attribute, and would not exist in
this form on a DPDK NIC. It is carried under a vendor-prefixed key
(`oaiPathHealthExt`), outside `PerfData`, precisely so that it cannot be mistaken for
a standard metric.

### Delay and packet loss are not available as 3GPP metrics

The SMF returns null for `ulDelays`, `dlDelays` and `rtDelays`. This is not an
oversight in this project: the SMF already implements the handlers and
`smf.qosmonlist` already carries the fields, but the UPF plugin does not implement the
TS 29.244 QoS-Monitoring information elements, so the values stay null.

An ICMP probe per N6 interface *does* measure delay, jitter and loss, but it crosses
the kernel veth path rather than the forwarding path, so it must never be reported as
a 3GPP KPI. **Treat the delay and loss fields as placeholders that read null, not as
supported metrics.**

### An idle path is indistinguishable from a healthy one

With no transmit attempt, nothing can fail. An impaired but idle path reads `UNKNOWN`,
not `DEGRADED`. This is structural, not a bug. It is why the anchor UE exists, and why
you must start traffic before impairing anything.

A hard failure that stops traffic entirely is invisible to **both** rules for the same
reason.

### Migration is paced, not capped

Serialization moves at most one session per evaluation cycle. It does not limit how
many move in total. While the source path looks degraded, sessions keep leaving it.
Partial migration does occur when the degradation is load-induced — the path recovers
as sessions leave — but that is emergent, not controllable.

### "Broken" and "overloaded" are indistinguishable

Both present as `OBSERVED_DEGRADED`, but the correct response differs: evacuate a
broken path, move only *some* sessions off an overloaded one. Separating them needs
per-session load plus a capacity model, and neither exists here.

### RATE steering is one-way

To move a session from A to B, B must be busier. After the move, A carries less and
can never win it back. RATE is a ratchet, not a control loop.

### Downlink throughput is capped by the UE simulator

About 1.4–4.3 Mbit/s, against roughly 270 Mbit/s uplink. This is gnbsim's GTP-U
receive path, not the network. Do not build a downlink metric on it.

### The data-network return path is a lab mechanism

`scripts/lab/nwdaf_dn_route_sync.py` keys the external data network's per-UE route off
the UPF's downlink PDR. It exists because the lab's external DN does not learn that a
UE's path moved. In a real deployment the UPF advertises the UE prefix out its active
N6 and the routing protocol converges. **This script is a lab substitute for data
network routing and is not part of 3GPP traffic steering.**

### The ML forecast engine is not on the steering path

`oai-nwdaf-engine-traffic-steering` is a separate experiment. Its source and models
are deliberately not part of this repository, and neither RATE nor HEALTH uses it. Its
absence is not an error, and `start_nwdaf.sh` skips it with a note.

* * *

## Development

### Where the changes are

| Area | Location |
|---|---|
| NWDAF services | `nwdaf/` — Go, built directly from this repository |
| OAI network functions | `patches/` — never vendored; each patch names the upstream commit it applies to |
| Deployment topology | `compose/` and `configs/` |
| Automation | `scripts/` |

The SMF carries three **new** source files that a patch cannot add, because a diff
cannot create an untracked file. They live in `patches/smf/` and are copied into the
upstream tree by `build.sh` before the patch adds them to the build:

```text
patches/smf/smf_nwdaf_consumer.cpp
patches/smf/smf_nwdaf_consumer.hpp
patches/smf/smf_nwdaf_steer_serialization.hpp
```

### How patches are applied

`scripts/build.sh nfs` does this for each network function:

1. Clone the upstream repository into `$SRC_DIR` (default `$HOME/oai-src`) if it is
   not already there.
2. Check out the exact base commit named in the patch header.
3. Apply the patches with `git apply`, checking first that they apply cleanly.
4. Build the image with the tag the deploy scripts expect.

If the upstream checkout already has local modifications, `build.sh` **builds it as
it stands and says so.** It will not discard your work.

### After making a change

```bash
make verify                # always - catches patch drift, syntax and build errors
make build-nwdaf           # if you changed anything under nwdaf/
make build-nfs             # if you changed anything under patches/ (HOURS)
```

Then redeploy the affected part:

| Changed | Redeploy with |
|---|---|
| NWDAF Go services | `make nwdaf` |
| SMF patch | `make core` |
| PCF policies under `configs/pcf-policies/` | `make build-fed` then `make ues` |
| Compose or `ulcl_config.yaml` | `make build-fed` then `make core` |

### Documentation

This repository keeps **`README.md` as its only Markdown file**. Please put
documentation here rather than adding new files.

* * *

## Reproducibility

The project is built so that a given checkout produces the same deployment on any
machine. Four mechanisms do this:

**Pinned upstream commits.** Every patch header names the exact upstream commit it
applies to, and `build.sh` checks that commit out before applying anything. The
commits are the contract:

| Component | Base commit |
|---|---|
| SMF | `667aa8fd356d2fd569bf8aa1c610065e7be18f65` |
| PCF | `67aee53df01542a0deccf32874bfb90b38c547b3` |
| NRF | `b38f13d82ba7885930f899993c0257940021cf40` |
| `oai-cn5g-fed` | `55859262fbcdaf6c89d8ccf9caed70ef432e4cb3` |

**Patches instead of vendored source.** The OAI network functions are not copied into
this repository. `patches/` shows exactly what changed and against what, which keeps
the delta reviewable and the licensing clean.

**Named image tags.** Each tag records a behaviour, and the deploy scripts check for
tags by name. A missing tag is reported as a missing tag rather than silently pulling
something else.

**Verification.** `make verify` re-checks all of the above on demand: that each patch
still applies to the commit it names, that the images still build, that every required
tag is present, and that the external prerequisites are in place.

What reproducibility does **not** cover: absolute throughput numbers, and the exact
timing of a steer. Both depend on the host's CPU and on how quickly the analytics
window fills.

* * *

## Quick Start

For someone who already has the [prerequisites](#prerequisites) installed. Building
the C++ network functions takes hours on a first run.

```bash
# clone
git clone https://github.com/Mragankk/oai-nwdaf-traffic-steering.git
cd oai-nwdaf-traffic-steering

# verify
make verify

# build            (build-nfs is the slow one - hours)
make build-nwdaf
make build-gnbsim
make build-fed
make build-nfs

# deploy
make core RULE=HEALTH
make nwdaf

# create UEs and traffic
make ues UES=5 ANCHORS=1
make load MBPS=60 SECS=1800 PROTO=udp

# run the HEALTH test
make test-health
make unsteer

# run the RATE test - it needs its own core and a different traffic shape
SMF_NWDAF_PREDICT_SEC=0 make core RULE=RATE
make nwdaf
make ues UES=3 ANCHORS=1
make load MBPS=60 SECS=1800 PROTO=udp
for c in gnbsim-vpp2 gnbsim-vpp3; do sudo docker exec $c pkill iperf3; done
make test-rate

# tear down
make clean
```

* * *

## Upstream and License

Built on [OpenAirInterface CN5G](https://gitlab.eurecom.fr/oai/cn5g). The OAI network
functions are **not vendored** — see [`patches/`](patches/) for exactly what changed
and against which commit. AMF, UPF-VPP, UDR, UDM and AUSF are used unmodified.

The NWDAF services under `nwdaf/` are modified from `oai-cn5g-nwdaf`.

Licensed under CSSL-1.0. See [`LICENSE`](LICENSE) and [`NOTICE`](NOTICE).

The UE credentials in `compose/` and `scripts/` (KEY, OPc, IMSI) are
OpenAirInterface's **public tutorial test vectors, not secrets.**
