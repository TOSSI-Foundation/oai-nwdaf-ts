# Testing — controlled path impairment

How to make a path genuinely degrade, so the telemetry can be validated against a known
cause rather than against hope.

## The N6 interfaces

VPP names its host interfaces after the compose `IF_n` index, so the mapping is
authoritative from the UPF's own environment — never guessed:

```
IF_3_TYPE=N6  IF_3_NWI=internet.oai.org.pri  IF_3_DNAI=internet-primary    → host-n6-3 / veth n6-3
IF_4_TYPE=N6  IF_4_NWI=internet.oai.org.sec  IF_4_DNAI=internet-secondary  → host-n6-4 / veth n6-4
```

Cross-check at runtime: the `upf-nwi-<nwi>` pseudo-interface's `ip4` count tracks the
matching `host-n6-N` tx packet count to within a few hundred.

> `vppctl` is at **`/openair-upf/bin/vppctl`** and is **not on `$PATH`** — plain
> `docker exec vpp-upf vppctl …` fails.

## Apply / inspect / clear

```bash
# impair the secondary path to 50 Mbit/s
sudo docker exec vpp-upf tc qdisc add dev n6-4 root tbf rate 50mbit burst 32kbit latency 400ms

# check — MUST read noqueue when clean
sudo docker exec vpp-upf tc qdisc show dev n6-3
sudo docker exec vpp-upf tc qdisc show dev n6-4

# remove
sudo docker exec vpp-upf tc qdisc del dev n6-4 root
```

**Always clear before finishing.** Both interfaces must read `qdisc noqueue` when idle.
`tc` runs in the UPF container's network namespace; the UPF binary and config are untouched.

## Reading the counters

```bash
sudo docker exec vpp-upf /openair-upf/bin/vppctl show errors | grep -E "Count|n6"
sudo docker exec vpp-upf /openair-upf/bin/vppctl show interface
```

Three distinct counters exist on a `host-n6-N-tx` node. Do not sum them:

| counter | behaviour |
|---|---|
| `tx sendto temporary failure` | ✅ the signal — fine-grained, monotonic per packet, CV ~8% |
| `tx frame not ready` | ⚠️ diagnostic only — quantised to 1024 (ring sweeps), non-monotonic, **reads zero at the most severe impairment** |
| `tx ring overrun` | ❌ no usable signal |
| interface `drops`, Linux `tx_dropped`/`tx_errors` | ❌ **zero under every impairment tested** |

VPP marks even informational counters with severity `error` (e.g. "good packets
decapsulated"), so never filter on severity.

## Expected response

Offered 120 Mbit/s, TBF on `n6-4`, healthy `n6-3` as a concurrent control:

| TBF | throughput | `sendto` /packet | `frame_not_ready` |
|---|---|---|---|
| none | 120.0 Mbit/s | **0** | 0 |
| 100M | 85.0 | 0.1425 | **0** |
| 50M | 40.5 | 0.2563 | 44,032 |
| 25M | 23.6 | 0.2882 | 52,224 |
| 10M | 10.9 | 0.3184 | **0** |

The healthy control read **exactly zero on every counter** in all ten healthy path-runs.
Absolute and per-second counts move *backwards* with severity — only per-packet and
per-byte are monotonic.

## Validating the four states

```bash
# idle          → UNKNOWN_NO_TRAFFIC            (no traffic)
# insufficient  → UNKNOWN_INSUFFICIENT_SAMPLES  (iperf3 -b 1M → ~450 pkts/interval)
# healthy       → OBSERVED_HEALTHY              (iperf3 -b 120M, no tc)
# degraded      → OBSERVED_DEGRADED             (iperf3 -b 120M + tc tbf 25mbit)

sudo docker exec oai-nwdaf-database mongosh --quiet --eval '
 const d=db.getSiblingDB("testing").upf_metrics.find({dnaiPerf:{$exists:true}}).sort({timestamp:-1}).limit(1).toArray()[0];
 for (const [k,v] of Object.entries(d.dnaiPerf))
   print(k+"  "+v.health.state+"  ratio="+v.health.sendtoFailurePerPacket+"  n="+v.health.txAttempts)'
```

Lower the floor with `PATH_HEALTH_MIN_ATTEMPTS` to exercise the insufficient case at
higher rates; set it to `0` to disable the floor entirely (restores pre-floor behaviour).

## What this is not

These counters are **AF_PACKET transmit stalls in a VPP-on-veth lab**. Not packet loss,
not `avgPacketLossRate`, not a 3GPP metric. `tc` shapes rather than drops, and TCP absorbs
it through backpressure and retransmission — it is entirely possible that a run loses zero
packets while the counter climbs.
