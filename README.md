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

## Status — read this before quoting results

| Claim | Status |
|---|---|
| `DN_PERFORMANCE` per DNAI, consumed by the SMF over `Nnwdaf_AnalyticsInfo` | ✅ working |
| PCF-authorized DNAI selection, PFCP FAR+PDR actuation | ✅ working |
| Per-DNAI N6 path health with an explicit UNKNOWN state | ✅ working |
| Steering **decision quality** | ⚠️ ranks on `avg_traffic_rate_bps` — **offered load, not path quality** |
| Reversibility | ⚠️ one-way: the ranking cannot return to an idle path |
| Packet delay / loss | 🔴 not measurable in this deployment |

The path-health signal is an **AF_PACKET transmit-stall indicator specific to this
VPP-on-veth lab**. It is **not** packet loss and **not** a 3GPP metric. See
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Layout

```
nwdaf/      NWDAF services (engine, nbi-analytics, nbi-events, sbi)
patches/    changes to upstream OAI NFs, pinned to base commits
compose/    deployment topology
configs/    NF config + PCF steering policies
scripts/    telemetry collector, DN route synchronizer, deploy helpers
docs/       architecture, quickstart, testing, full engineering log
```

## Upstream

Built on [OpenAirInterface CN5G](https://gitlab.eurecom.fr/oai/cn5g). The OAI network
functions are **not vendored** — see [patches/](patches/) for exactly what changed and
against which commit. AMF, UPF-VPP, UDR, UDM and AUSF are used **unmodified**.

Start with [docs/QUICKSTART.md](docs/QUICKSTART.md).
