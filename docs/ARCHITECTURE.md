# Architecture

## The loop

```
                    ┌──────────────────────────────┐
                    │  PCF — authorization only    │
                    │  SmPolicyDecision → DNAI set │
                    └───────────────┬──────────────┘
                                    │ authorized DNAIs (per session, per precedence)
                                    ▼
 NWDAF ──Nnwdaf_AnalyticsInfo──►  SMF  smf_nwdaf_consumer
   │                                │   · confidence floor  (SMF_NWDAF_MIN_CONFIDENCE, 50)
   │                                │   · rank              (avg_traffic_rate_bps)
   │                                │   · hysteresis margin (SMF_NWDAF_DNPERF_MARGIN_PERCENT, 10)
   │                                ▼
   │                       choose_authorized_dnai()   ← the PCF always wins
   │                                │
   │                                ▼  PFCP Session Modification
   │                     Update FAR (uplink egress) + Update PDR (downlink match)
   │                                │
   │                                ▼
   │                        VPP-UPF: NWI → FIB → host-n6-N
   │                                │
   └──── usage reports ─────────────┘
         + N6 interface counters
```

Both PFCP rules live on the **same N6 edge** and must move together. Updating only the
FAR moves uplink egress while the downlink PDR still matches the old network instance,
and return traffic is silently dropped.

## Two data sources, deliberately not merged

| | `PerfData` (3GPP) | `oaiPathHealthExt` (vendor extension) |
|---|---|---|
| source | PFCP usage reports | UPF N6 interface counters |
| scope | per (DNN, S-NSSAI, DNAI), joined per SUPI | **per DNAI / per path** |
| window | 300 s average | one ~5 s poll interval |
| session-less DNAI | **absent entirely** | present, with an honest state |

They are at different time scales and are *expected* to disagree. A DNAI can show
`avgTrafficRate = 15.5 Mbps` (300 s average) and `UNKNOWN_INSUFFICIENT_SAMPLES`
(this interval) simultaneously — both correct.

## Path-health states

```
txAttempts == 0                            → UNKNOWN_NO_TRAFFIC
0 < txAttempts < PATH_HEALTH_MIN_ATTEMPTS  → UNKNOWN_INSUFFICIENT_SAMPLES
txAttempts >= floor, failures == 0         → OBSERVED_HEALTHY
txAttempts >= floor, failures > 0          → OBSERVED_DEGRADED
                              (+ UNKNOWN_NO_SAMPLE, UNKNOWN_STALE)
```

**UNKNOWN is not healthy.** An idle path makes no transmit attempt, so it *cannot*
produce a failure — an idle impaired path is byte-for-byte identical to an idle healthy
one. Any consumer treating "no failures" as "good" will select a path it knows nothing
about. `sendtoFailurePerPacket` is `null`, never `0`, whenever the state is UNKNOWN.

### What the metric actually is

`sendtoFailurePerPacket` = `tx sendto temporary failure` delta ÷ `tx packets` delta, at
the **AF_PACKET boundary between VPP and a Linux veth**. It counts transmit *stalls*
(the kernel returned EAGAIN/ENOBUFS), not discarded packets — under every impairment
measured, VPP interface `drops`, Linux `tx_dropped` and `tx_errors` all stayed at
exactly **zero**.

It is **not** `avgPacketLossRate`, not any 3GPP attribute, and would not exist in this
form on a DPDK NIC. It is carried under a vendor-prefixed key, outside `PerfData`, so it
cannot be mistaken for one.

**Why per-packet:** across a severity sweep the absolute and per-second counts move
*backwards* as impairment worsens — a harder-limited path issues fewer `sendto()` calls,
so fewer fail. Only the per-attempt and per-byte forms are monotonic.

**`PATH_HEALTH_MIN_ATTEMPTS = 1000`:** with zero failures in *n* attempts the 95% upper
bound on the true stall rate is ≈ 3/n, so n=1000 gives 0.003 — ~43× below the mildest
sustained degradation measured. Costs a ~2.4 Mbit/s observability floor. When in doubt
this floor goes **up**, never down: a false `OBSERVED_HEALTHY` is unrecoverable,
`UNKNOWN` is not.

## Known limitations

1. **The ranking metric is offered load.** `argmax(avg_traffic_rate_bps)` — a path
   carrying more bits wins. For congestion avoidance that is backwards.
2. **Steering is one-way.** To move A→B, B must be busier; after the move A falls to
   zero, so it can never win back. A ratchet, not a control loop.
3. **Delay and loss are unavailable.** The SMF returns null for `ulDelays`/`dlDelays`/
   `rtDelays`; TS 23.288 Table 6.4.2-2 NOTE 1 leaves collection undefined in this release.
4. **Downlink is capped by the UE simulator** at ~1.4–4.3 Mbit/s against ~270 Mbit/s
   uplink — gnbsim's GTP-U receive path, not the network.
5. **The DN return path is a lab mechanism.** See below.

## The DN route synchronizer — not 3GPP

`scripts/lab/nwdaf_dn_route_sync.py` keys the external DN's per-UE route off the UPF's
**downlink PDR** — the rule return traffic must actually satisfy — so the route cannot
disagree with the dataplane.

It exists because the lab's external DN does not learn that a UE's path moved. In a real
deployment the PSA UPF advertises the UE prefix out its active N6 and the DN's routing
protocol converges; TS 23.501 §5.6.4 (ULCL) is the standards mechanism for genuinely
different local exits. **This script is a lab substitute for DN routing and is not part
of 3GPP traffic steering.**

Without it, a steer breaks connectivity in **both** directions — TCP cannot establish
without the return path. Verified by controlled experiment.
