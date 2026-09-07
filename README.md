# NWDAF-driven DNAI traffic steering on OAI 5G SA

An NWDAF that measures per-path performance, and an SMF that acts on it — moving a
**live PDU session** between two N6 data-network paths without touching the UE.

```
UPF usage reports + N6 interface counters
        │
        ▼
NWDAF  ──  DN_PERFORMANCE (per DNAI)  +  per-DNAI path health
        │
        ▼  Nnwdaf_AnalyticsInfo
SMF    ──  selects a DNAI from the set the PCF authorized
        │
        ▼  PFCP Session Modification (Update FAR + Update PDR)
UPF    ──  N6 egress moves to the other network instance
```

The PCF stays the authorization authority; the NWDAF only ever *proposes*, and the
SMF only ever selects **within** what the PCF allowed.

## What this demonstrates

Same SUPI, same PDU session, **same UE IP**, path changes underneath — validated with
0 new PDU sessions, 0 re-registrations and 0% packet loss in both directions.

## Status - read this before quoting results

Working today:

| Part | State |
|---|---|
| `DN_PERFORMANCE` per DNAI, consumed by the SMF over `Nnwdaf_AnalyticsInfo` | Working |
| PCF-authorized DNAI selection, PFCP FAR + PDR actuation | Working |
| Per-DNAI N6 path health, with an explicit UNKNOWN state | Working |
| Live session moved with no re-attach and no IP change | Working |

Known limits, and whether they can be fixed:

| Limit | What it means | Status |
|---|---|---|
| Packet delay and packet loss are not available as 3GPP metrics | The SMF returns null for delay, and nothing in this dataplane counts a dropped packet. | Partly. The standards path is genuinely blocked: `smf.qosmonlist` already carries `uldelays`/`dldelays`/`rtdelays` and the SMF already implements the handlers, but the UPF plugin does not implement the TS 29.244 QoS-Monitoring IEs, so the fields stay null. An ICMP probe per N6 interface does measure delay, jitter and loss (78 ms / 11.8 ms / 6.7 % on an impaired path vs 0.2 / 0.2 / 0 healthy) — but it crosses the kernel veth path, not the forwarding path, so it must never be reported as a 3GPP KPI. |
| Migration is paced but not capped | Serialization moves one session per cycle; it does not limit how many move in total. While the source path looks degraded, sessions keep leaving it. | Partly, by feedback. When degradation is load-induced the path recovers as sessions leave and the rest hold — observed holding for ~95 s. But it is emergent, not controllable, and a single transient degraded sample can release the last session. |
| "Broken" and "overloaded" are indistinguishable | Both present as `OBSERVED_DEGRADED`, but the right response differs: evacuate a broken path, move only some sessions off an overloaded one. | No. Separating them needs per-session load plus a capacity model, neither of which exists here. |
| A hard failure that stops traffic is invisible | No transmit attempt can fail, so the path reads `UNKNOWN`, not `DEGRADED`. | No. Structural: an idle impaired path is byte-for-byte identical to an idle healthy one. |

Two limits listed in earlier revisions are **fixed**, both opt-in via
`SMF_NWDAF_DNPERF_RULE=HEALTH` (the default stays `RATE`, the original behaviour):

| Was | Now |
|---|---|
| Ranked on `avg_traffic_rate_bps` — offered load, not path quality | The `HEALTH` rule gates on per-DNAI path health and steers only **away** from a path observed degraded. Detection dropped from a 300 s average to a 5 s interval, and steer latency from minutes to ≤10 s. |
| One-way: could not return to an idle path | Reversible. UNKNOWN is never read as healthy, and a decaying memory of recently-degraded paths prevents flapping. A→B→A driven by measurement, validated. |

See [docs/MULTI-UE-STEERING.md](docs/MULTI-UE-STEERING.md).

The path-health signal is an **AF_PACKET transmit-stall indicator specific to this
VPP-on-veth lab**. It is **not** packet loss and **not** a 3GPP metric. See
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Quick start

```bash
./scripts/build.sh          # build all 9 images (NWDAF ~2 min; the C++ NFs are hours)
make up                     # core + NWDAF + UEs, end to end
make load                   # traffic on every UE
make test-health            # automated HEALTH steering test  -> PASS/FAIL
make test-rate              # automated RATE steering test    -> PASS/FAIL
make clean                  # tear down (keeps the MongoDB volume)
```

Nothing is published to a registry, so `build.sh` is not optional — see
[docs/QUICKSTART.md](docs/QUICKSTART.md) for prerequisites and the image table.

## Layout

```
nwdaf/      NWDAF services (engine, nbi-analytics, nbi-events, sbi)
patches/    changes to upstream OAI NFs, pinned to base commits
compose/    deployment topology
configs/    NF config + PCF steering policies
scripts/    build.sh, deploy helpers, telemetry collector, DN route sync, tests
docs/       architecture, networks, quickstart, testing
```

| Doc | What is in it |
|---|---|
| [QUICKSTART.md](docs/QUICKSTART.md) | prerequisites, building the images, bring-up, first test |
| [ARCHITECTURE.md](docs/ARCHITECTURE.md) | the control loop, the two data sources, what the metric is not |
| [NETWORKS.md](docs/NETWORKS.md) | every subnet, address and port, and which N6 veth is which DNAI |
| [MULTI-UE-STEERING.md](docs/MULTI-UE-STEERING.md) | the HEALTH rule, serialization, full configuration reference |
| [TESTING.md](docs/TESTING.md) | controlled path impairment and how to read the counters |

## Upstream

Built on [OpenAirInterface CN5G](https://gitlab.eurecom.fr/oai/cn5g). The OAI network
functions are **not vendored** — see [patches/](patches/) for exactly what changed and
against which commit. AMF, UPF-VPP, UDR, UDM and AUSF are used **unmodified**.

Start with [docs/QUICKSTART.md](docs/QUICKSTART.md).
