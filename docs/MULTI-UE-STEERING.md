# Multi-UE steering: the HEALTH rule and serialization

Two behaviours that only appear with more than one steerable session. Both are
**opt-in**: `SMF_NWDAF_DNPERF_RULE` defaults to `RATE`, which is the original
`argmax(avgTrafficRate)` selection.

---

## 1. Why the default rule is not congestion avoidance

`RATE` ranks on `avgTrafficRate` — **offered load, not path quality** — so it
steers *toward* the busier path. Measured: primary idle at `0 bps`, secondary
carrying `210.90 Mbps`, decision = move the session **onto** secondary, leaving
one path empty and both UEs on the other.

It is also a **one-way ratchet**: to move A→B, B must be busier; after the move A
falls to zero and can never win back.

Set `SMF_NWDAF_DNPERF_RULE=HEALTH` for the path-health rule instead.

---

## 2. The HEALTH rule

```
h = health(the session's UPF-CONFIRMED DNAI), rejected as UNKNOWN if older than 30 s

h == OBSERVED_DEGRADED  -> steer to the best authorized alternative
h == OBSERVED_HEALTHY   -> HOLD
h == UNKNOWN_*          -> HOLD          (unknown is NOT bad)
```

Target selection among authorized DNAIs other than the current one:

1. exclude `OBSERVED_DEGRADED`
2. exclude **recently degraded** — degraded within
   `SMF_NWDAF_DEGRADED_MEMORY_SEC` (120 s) and not seen healthy since. This is
   what stops A→B→A ping-pong: steer off a degraded path and it goes idle,
   whereupon it reads `UNKNOWN_NO_TRAFFIC` — indistinguishable from healthy — and
   a rule without memory would steer straight back
3. prefer `OBSERVED_HEALTHY` over `UNKNOWN_*`
4. within a tier, lowest `avgTrafficRate`. **The confidence floor applies only
   here**, the one place a rate is still used

**`state` is the only signal that gates the decision.** Rate and confidence are
carried and logged but, with two DNAIs, never reach step 4 — there is only ever
one candidate.

### How `state` is computed

Not by the NWDAF — by the host collector, every 5 s per N6 interface:

```python
attempts = delta["tx packets"]
failures = delta["txErrors"]["tx sendto temporary failure"]

attempts == 0                  -> UNKNOWN_NO_TRAFFIC            (ratio null)
attempts < PATH_HEALTH_MIN_ATTEMPTS (1000)
                               -> UNKNOWN_INSUFFICIENT_SAMPLES  (ratio null)
failures == 0                  -> OBSERVED_HEALTHY
failures  > 0                  -> OBSERVED_DEGRADED
```

`tx sendto temporary failure` is `sendto()` returning EAGAIN/ENOBUFS — the
AF_PACKET TX ring is full, i.e. real egress backpressure. **It is not packet
loss**: VPP `drops`, Linux `tx_dropped` and `tx_errors` all stayed at exactly
zero under every impairment measured. It is a lab-specific transmit-stall
indicator, never `avgPacketLossRate`, and it travels in the vendor-namespaced
`oaiPathHealthExt` for that reason.

The healthy/degraded boundary is **"any non-zero"**, not a tuned threshold:
across 197 samples the counter was either exactly 0 or ≥340.

---

## 3. Serialization — one steer per evaluation cycle

Without it, every session on a degraded path reaches the same verdict in the same
poll and they all migrate together. Measured with 5 UEs: four sessions moved
inside one 10 s poll. The per-session cooldown cannot prevent that — it is *per
session* and they all fire at once.

`evaluate_sessions()` is called exactly once per poll, so a budget local to it is
global to the steering engine. Two gates:

```
1. BUDGET     steers_this_cycle >= SMF_NWDAF_STEER_MAX_PER_CYCLE (default 1)
2. FRESHNESS  target health observedAt <= the last steer  -> hold
```

The freshness gate is the half that makes this closed-loop rather than merely
rate-limited: the SMF polls every 10 s while health up to 30 s old is accepted,
so a later cycle can otherwise read a sample taken *before* the previous move and
showing none of its effect.

Budget is consumed by a **real** steer only — `maybe_trigger_steering()` declines
several no-op paths, and counting those would let one no-op block a genuine move
for a whole cycle.

Observed with 4 steerable sessions:

```
poll3:   3 eligible session(s), 1 steered
poll8:   2 eligible session(s), 0 steered     (held)
poll14:  2 eligible session(s), 1 steered
poll19:  1 eligible session(s), 1 steered
MAX steers in any single cycle: 1
```

---

## 4. Configuration

| variable | default | meaning |
|---|---|---|
| `SMF_NWDAF_DNPERF_RULE` | `RATE` | `HEALTH` selects the path-health rule |
| `SMF_NWDAF_STEER_MAX_PER_CYCLE` | `1` | steers per evaluation cycle; `0` disables the limit |
| `SMF_NWDAF_HEALTH_MAX_AGE_SEC` | `30` | consumer-side freshness bound |
| `SMF_NWDAF_DEGRADED_MEMORY_SEC` | `120` | how long a DNAI stays suspect after being seen degraded |
| `SMF_NWDAF_STEER_COOLDOWN_SEC` | `60` | minimum interval between two steers of the same session |
| `PATH_HEALTH_MIN_ATTEMPTS` | `1000` | collector: tx packets needed before a health verdict |
| `MONGODB_QOSMON_RETAIN` | `1000` | collector: usage reports kept per SUPI; `0` = unbounded |

---

## 5. Running it

```bash
bash scripts/lab/demo_reset_multi.sh 5 1     # 5 UEs: 4 steerable + 1 anchor
bash scripts/lab/demo_load_multi.sh 60 1800 udp
sudo docker exec vpp-upf tc qdisc add dev n6-3 root tbf rate 40mbit burst 64kbit latency 400ms
# ... watch, then ALWAYS:
sudo docker exec vpp-upf tc qdisc del dev n6-3 root
```

`demo_reset_multi.sh` takes `[N_UES] [N_ANCHORS]`. The last `N_ANCHORS` UEs are
pinned to secondary and exist to keep that path **measurable** — without one, the
abandoned path only ever reads `UNKNOWN`.

Watch with:

```bash
sudo docker logs -f oai-smf 2>&1 | grep --line-buffered -E \
  "Steering cycle:|per-session decision|Steering: Update|dnai="
```

### Things that will waste your time

* **Use UDP for impairment tests.** TCP's control connection collapses under a
  sustained rate limit and the flow dies, which looks exactly like "no traffic"
  and makes the path report `UNKNOWN` instead of `OBSERVED_DEGRADED`.
* **Each UE must target the DN leg on its own path.** A session *established* on
  secondary connects to the primary DN leg over UDP and then transmits nothing,
  silently. `demo_load_multi.sh` derives the mapping.
* **A leftover `tc` qdisc silently breaks every later test.**
* **Wait for both paths to read `0 bps` before resetting** — the engine averages
  over 300 s and MongoDB is not restarted, so an immediate reset re-steers on
  stale data.

---

## 6. Known limits

* A hard failure that stops traffic is invisible to **both** rules: no transmit
  attempt can fail, so the path reads `UNKNOWN`, not `DEGRADED`.
* Migration is **not capped**. Serialization paces it; it does not limit the
  total. Sessions keep moving while the source looks degraded. Partial migration
  does occur when degradation is load-induced — the path recovers as sessions
  leave — but it is emergent, not controllable, and a single transient degraded
  sample can release the last session.
* The system **cannot distinguish "broken" from "overloaded"**. Both present as
  `OBSERVED_DEGRADED`, and the correct response differs.
* Delay, jitter and packet loss are not collected.
