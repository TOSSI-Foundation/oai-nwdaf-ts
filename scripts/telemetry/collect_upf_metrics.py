#!/usr/bin/env python3
# SPDX-License-Identifier: LicenseRef-CSSL-1.0
"""
Host-side poller for UPF resource metrics (CPU/memory), feeding the NF_LOAD
analytic and the traffic-steering engine.

Runs outside any container: it reads the target UPF container's cgroup v2
accounting files directly from the host (`docker stats` misreports 0 for the
privileged/DPDK vpp-upf container - confirmed during discovery), so no change
to the oai-cn5g-fed deployment is required. Requires passwordless `sudo` for
`docker inspect`/`cat` against the fed stack's containers (read-only).

It ALSO collects per-DNAI N6 interface counters from the running VPP UPF.
That part is strictly additive and
never fatal: if `vppctl` cannot be reached the document is written exactly as
before, so the NF_LOAD analytic and the traffic-steering engine are unaffected.
Set UPF_VPP_COUNTERS=0 to disable it entirely.

RAW ONLY, DELIBERATELY. This stores counters and their per-interval deltas and
derives NOTHING. In particular it does NOT compute a packet-loss rate: measured
at ~180 Mbit/s the interface `drops` and `tx sendto temporary failure` counters
did not move at all, so calling them loss would be inventing a signal. What the
semantics actually are, and which of these can legitimately support a metric,
is Improvement 4's job - after this has collected enough evidence to decide.
"""

import argparse
import logging
import os
import re
import subprocess
import sys
import time

from pymongo import MongoClient

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)s %(message)s",
)
log = logging.getLogger("collect_upf_metrics")


def env(name, default):
    return os.environ.get(name, default)


UPF_CONTAINER_NAME = env("UPF_CONTAINER_NAME", "vpp-upf")
UPF_ID = env("UPF_ID", UPF_CONTAINER_NAME)
MONGODB_URI = env("MONGODB_URI", "mongodb://localhost:27017")
MONGODB_DATABASE_NAME = env("MONGODB_DATABASE_NAME", "testing")
MONGODB_COLLECTION_UPF_METRICS = env("MONGODB_COLLECTION_UPF_METRICS", "upf_metrics")
MONGODB_COLLECTION_SMF = env("MONGODB_COLLECTION_SMF", "smf")
POLL_INTERVAL_SEC = int(env("POLL_INTERVAL_SEC", "5"))
# Window for counting ACTIVE PDU sessions. Must exceed the SMF's usage-report
# period (~10 s here) - see derive_smf_features().
ACTIVE_SESSION_WINDOW_SEC = int(env("ACTIVE_SESSION_WINDOW_SEC", "30"))


def run(cmd):
    """Run cmd, transparently retrying with `sudo -n` on permission errors."""
    result = subprocess.run(cmd, capture_output=True, text=True)
    if result.returncode != 0 and "permission denied" in (result.stderr or "").lower():
        result = subprocess.run(["sudo", "-n"] + cmd, capture_output=True, text=True)
    if result.returncode != 0:
        raise RuntimeError(f"command failed: {' '.join(cmd)}: {result.stderr.strip()}")
    return result.stdout


def get_container_pid(container_name):
    out = run(["docker", "inspect", "--format", "{{.State.Pid}}", container_name])
    pid = out.strip()
    if not pid or pid == "0":
        raise RuntimeError(f"container {container_name} is not running")
    return pid


def get_cgroup_path(pid):
    out = run(["cat", f"/proc/{pid}/cgroup"])
    # cgroup v2 single-hierarchy line looks like: "0::/system.slice/docker-<id>.scope"
    for line in out.splitlines():
        parts = line.split(":", 2)
        if len(parts) == 3 and parts[0] == "0":
            return parts[2]
    raise RuntimeError(f"could not parse cgroup v2 path from: {out!r}")


def read_cpu_stat(cgroup_path):
    out = run(["cat", f"/sys/fs/cgroup{cgroup_path}/cpu.stat"])
    stats = {}
    for line in out.splitlines():
        key, _, value = line.partition(" ")
        if key in ("usage_usec", "user_usec", "system_usec"):
            stats[key] = int(value)
    return stats


def read_memory_current(cgroup_path):
    out = run(["cat", f"/sys/fs/cgroup{cgroup_path}/memory.current"])
    return int(out.strip())


# ---------------------------------------------------------------------------
# Per-DNAI N6 interface counters, read from the running VPP UPF.
#
# WHY THIS EXISTS: every per-path number this project had came from PFCP usage
# reports, i.e. from the SESSIONS on a path, which measures offered load and
# goes completely dark when a path is abandoned. Interface counters measure the
# PATH. They do not solve the idle-path problem on their own (an idle N6
# interface reads exactly zero - measured), but they are the only per-DNAI
# observation available without forking the UPF, which is the project's standing
# constraint.
#
# THE MAPPING IS NOT GUESSED. The UPF container's own environment declares
# IF_n_TYPE / IF_n_NWI / IF_n_DNAI, and VPP names its host interfaces
# "host-<type>-<n>" using the SAME index, so IF_3_TYPE=N6 is host-n6-3. Each
# poll also records `upf-nwi-<nwi>`'s own ip4 counter as an independent
# cross-check: it tracks the matching host-n6-N tx packet count to within a few
# hundred, so a mismatch means the mapping assumption has broken.
VPP_CTL = env("VPP_CTL", "/openair-upf/bin/vppctl")  # NOT on $PATH in the image
# Minimum transmit attempts in one interval before a health VERDICT may be given.
#
# Below this the interval is reported UNKNOWN_INSUFFICIENT_SAMPLES rather than
# OBSERVED_HEALTHY, because "no failures out of 3 packets" is not evidence of
# health - it is an empty window that happens to round to zero. Observed live:
# three OBSERVED_HEALTHY samples with txAttempts of 1, 1 and 3.
#
# 1000 is chosen statistically, not fitted. With zero failures in n attempts the
# 95% upper bound on the true stall rate is ~3/n (rule of three), so n=1000 puts
# it at 0.003 - about 43x below 0.1287, the mildest SUSTAINED degradation ever
# measured here. It costs a ~2.4 Mbit/s observability floor at 1500 B packets on
# a 5 s poll, and the observed data has a three-order-of-magnitude gap between
# {1,1,3} and the next sample at 2476, so anything in [4, 2476) behaves
# identically on real data - the value is not sensitive, and 1000 sits inside
# that gap with the largest defensible statistical margin.
#
# The failure direction matters: a false OBSERVED_HEALTHY sends a consumer into
# a path it knows nothing about, whereas UNKNOWN is
# recoverable. When in doubt this floor should go UP, never down.
PATH_HEALTH_MIN_ATTEMPTS = int(env("PATH_HEALTH_MIN_ATTEMPTS", "1000"))
COLLECT_VPP_COUNTERS = env("UPF_VPP_COUNTERS", "1") != "0"

# Counters worth keeping. "drops" and the tx-failure counter are kept because
# they are the ones a loss-like metric would eventually be built from - not
# because they currently carry signal (they do not; see the module docstring).
_VPP_COUNTER_RE = re.compile(
    r"(rx packets|rx bytes|tx packets|tx bytes|drops|punt|ip4|ip6"
    r"|rx-miss|rx-error|tx-error)\s+(\d+)\s*$"
)
_VPP_ERROR_RE = re.compile(r"^\s*(\d+)\s+(\S+)\s+(.+?)\s+(error|warn|info)\s*$")
_VPP_RUNTIME_RE = re.compile(
    r"^(\S+)\s+\S+\s+(\d+)\s+(\d+)\s+\d+\s+\S+\s+([\d.]+)\s*$"
)

# Previous raw sample, so each poll can publish a delta as well as the running
# total. VPP counters are cumulative since process start and can only be reset
# globally (`clear interfaces`), which would corrupt every other consumer - so
# deltas are computed here and the counters are never cleared.
_vpp_prev = {"ts": None, "counters": {}}

# GAUGES, not counters. These are instantaneous readings that legitimately go
# DOWN, so they must be excluded from both the delta arithmetic and the
# "counters went backwards means VPP restarted" guard below. Found the hard way:
# vectorsPerCall falling 2.29 -> 2.08 as load eased was read as a restart, and
# the whole sample's delta was discarded - which would have blanked the data
# exactly when offered load DROPS, the case a ranking rule most needs to see.
_VPP_GAUGES = frozenset({"vectorsPerCall"})


def vppctl(args):
    return run(["docker", "exec", UPF_CONTAINER_NAME, VPP_CTL] + args)


def n6_interfaces():
    """[(interface, nwi, dnai)] for every N6 interface the UPF declares."""
    out = run([
        "docker", "inspect", UPF_CONTAINER_NAME,
        "--format", "{{range .Config.Env}}{{println .}}{{end}}",
    ])
    cfg = {}
    for line in out.splitlines():
        if "=" in line:
            key, _, value = line.partition("=")
            cfg[key.strip()] = value.strip()

    found = []
    for idx in range(1, 32):
        if cfg.get(f"IF_{idx}_TYPE") != "N6":
            continue
        nwi = cfg.get(f"IF_{idx}_NWI")
        dnai = cfg.get(f"IF_{idx}_DNAI")
        if nwi and dnai:
            found.append((f"host-n6-{idx}", nwi, dnai))
    return found


def parse_show_interface(text):
    """{interface: {counter: value}} from `vppctl show interface`."""
    result = {}
    current = None
    for line in text.splitlines():
        if not line.strip():
            continue
        if not line[0].isspace():
            current = line.split()[0]
            result.setdefault(current, {})
        if current is None:
            continue
        match = _VPP_COUNTER_RE.search(line)
        if match:
            result[current][match.group(1)] = int(match.group(2))
    return result


def parse_show_errors(text):
    """{node: {reason: count}} from `vppctl show errors`."""
    result = {}
    for line in text.splitlines():
        match = _VPP_ERROR_RE.match(line)
        if match:
            count, node, reason = int(match.group(1)), match.group(2), match.group(3).strip()
            result.setdefault(node, {})[reason] = count
    return result


def parse_show_runtime(text):
    """{node: (calls, vectors, vectors_per_call)} keeping the BUSIEST thread.

    The dataplane runs on the worker thread (vpp_wk_0), not vpp_main, so a
    naive first-match would read the idle main thread's row. Vectors/call is
    VPP's own load indicator: ~1.0 idle, up to 256 when saturated.
    """
    result = {}
    for line in text.splitlines():
        match = _VPP_RUNTIME_RE.match(line.rstrip())
        if not match:
            continue
        node, calls, vectors, per_call = (
            match.group(1), int(match.group(2)), int(match.group(3)), float(match.group(4))
        )
        if node not in result or calls > result[node][0]:
            result[node] = (calls, vectors, per_call)
    return result


# ---------------------------------------------------------------------------
# Path-health state for one DNAI, derived from ONE interval's delta.
#
# WHAT THE METRIC IS - and what it is emphatically not.
#
# `sendtoFailurePerPacket` is the fraction of this interval's transmit
# attempts on this N6 interface that VPP could not hand to the kernel socket:
#
#     tx_sendto_temporary_failure delta  /  tx packets delta
#
# The denominator is named explicitly in the document (`txAttempts`) and the
# raw numerator and denominator are BOTH retained, so the metric can be
# recomputed or replaced later without re-running any experiment.
#
# It is an AF_PACKET transmit-side backpressure/stall indicator specific to
# this VPP-on-veth deployment. It is NOT packet loss, NOT a loss rate, and NOT
# any 3GPP metric - in every impairment run measured, interface `drops`,
# `lnx_tx_dropped` and `lnx_tx_errors` stayed at exactly zero, so nothing here
# counts a discarded packet. On a DPDK NIC these counters would not exist in
# this form.
#
# WHY /packet AND NOT /second OR AN ABSOLUTE COUNT. Measured across a TBF
# severity sweep (none/100/50/25/10 Mbit/s), the absolute and per-second counts
# move BACKWARDS as impairment worsens - a harder-limited path makes fewer
# sendto() calls, so fewer fail. Only the per-attempt and per-byte forms are
# monotonic. Per-packet is preferred over per-byte because its denominator is
# the transmit attempt itself; the two happen to be equivalent here only
# because MTU is effectively fixed, which is NOT true in general.
#
# THE UNKNOWN STATE IS THE POINT. A path with no transmit attempts cannot
# produce a failure, so an idle impaired path and an idle healthy path are
# byte-for-byte identical - verified: both read zero on every counter. Zero
# observations therefore must NEVER be reported as a zero failure rate, or a
# consumer would read "idle" as "healthy" and steer straight into a path it has
# no information about.
STATE_UNKNOWN_NO_SAMPLE = "UNKNOWN_NO_SAMPLE"   # no interval to compare against yet
STATE_UNKNOWN_NO_TRAFFIC = "UNKNOWN_NO_TRAFFIC"
# Traffic existed, but too little of it to support a verdict either way.
STATE_UNKNOWN_INSUFFICIENT = "UNKNOWN_INSUFFICIENT_SAMPLES"
STATE_OBSERVED_HEALTHY = "OBSERVED_HEALTHY"
STATE_OBSERVED_DEGRADED = "OBSERVED_DEGRADED"

SENDTO_FAILURE_REASON = "tx_sendto_temporary_failure"


def path_health(delta):
    """Health of one DNAI for one interval, or UNKNOWN when unobservable."""
    attempts = delta.get("tx packets", 0)
    failures = delta.get("txErrors", {}).get(SENDTO_FAILURE_REASON, 0)

    health = {
        "txAttempts": attempts,
        "sendtoFailures": failures,
        # Published so the threshold that produced the verdict travels with it -
        # a consumer can then re-derive the state under its own floor.
        "minAttempts": PATH_HEALTH_MIN_ATTEMPTS,
        "denominator": "tx packets transmitted on this N6 interface in the interval",
        "semantics": (
            "AF_PACKET transmit-side backpressure/stall indicator for this "
            "VPP-on-veth deployment; NOT packet loss and NOT a 3GPP metric"
        ),
    }

    if attempts <= 0:
        # No transmit attempt means no failure CAN occur. Absent, never zero.
        health["state"] = STATE_UNKNOWN_NO_TRAFFIC
        health["sendtoFailurePerPacket"] = None
        return health

    if attempts < PATH_HEALTH_MIN_ATTEMPTS:
        # Some traffic, but not enough to distinguish a healthy path from a
        # degraded one. The ratio is suppressed rather than published, for the
        # same reason UNKNOWN_NO_TRAFFIC suppresses it: a consumer reading the
        # number would treat a near-empty window as a measurement. The raw
        # counts stay, so the verdict can be recomputed under a different floor.
        health["state"] = STATE_UNKNOWN_INSUFFICIENT
        health["sendtoFailurePerPacket"] = None
        return health

    rate = failures / attempts
    health["sendtoFailurePerPacket"] = rate
    # The healthy/degraded boundary is "any non-zero", not a tuned threshold:
    # across every healthy run measured - including both DNAIs under matched
    # concurrent load - this counter was a hard zero. A consumer that wants a
    # tolerance band should apply it to sendtoFailurePerPacket itself; this
    # layer only reports what was observed.
    health["state"] = (
        STATE_OBSERVED_HEALTHY if failures == 0 else STATE_OBSERVED_DEGRADED
    )
    return health


def collect_vpp_dnai_counters():
    """{dnai: {...}} raw per-DNAI N6 counters plus a per-interval delta.

    Returns {} and logs a warning if the UPF CLI is unreachable - the caller
    treats that as "no VPP data this poll", never as a failure.
    """
    interfaces = n6_interfaces()
    if not interfaces:
        log.warning("no N6 interfaces declared in the %s environment", UPF_CONTAINER_NAME)
        return {}

    counters = parse_show_interface(vppctl(["show", "interface"]))
    errors = parse_show_errors(vppctl(["show", "errors"]))
    runtime = parse_show_runtime(vppctl(["show", "runtime"]))

    now = time.time()
    prev_ts = _vpp_prev["ts"]
    prev = _vpp_prev["counters"]
    fresh = {}
    out = {}

    for interface, nwi, dnai in interfaces:
        raw = dict(counters.get(interface, {}))
        if not raw:
            log.warning("VPP reports no counters for %s (DNAI %s)", interface, dnai)
            continue

        # EVERY tx-node error reason, kept SEPARATELY and verbatim.
        #
        # This replaced a substring sum over reasons matching "failure" or
        # "error". That sum was wrong twice over: VPP marks even informational
        # counters with severity "error" (e.g. "good packets decapsulated"), so
        # the predicate is meaningless; and it silently captured only
        # "tx sendto temporary failure" while discarding "tx frame not ready",
        # which is ~4x larger.
        tx_node = f"{interface}-tx"
        raw["txErrors"] = {
            reason.lower().replace(" ", "_"): count
            for reason, count in errors.get(tx_node, {}).items()
        }
        if tx_node in runtime:
            raw["txCalls"], raw["txVectors"], raw["vectorsPerCall"] = runtime[tx_node]

        # Independent check that host-n6-N really is this NWI: the pseudo
        # interface named after the network instance counts the same packets.
        nwi_counters = counters.get(f"upf-nwi-{nwi}", {})
        nwi_ip4 = nwi_counters.get("ip4")

        entry = {
            "nwi": nwi,
            "interface": interface,
            "raw": raw,
            "nwiIp4": nwi_ip4,
            "delta": None,
            # Overwritten below once an interval exists. Never absent, so a
            # consumer always finds an explicit state rather than inferring
            # health from a missing key.
            "health": {
                "state": STATE_UNKNOWN_NO_SAMPLE,
                "sendtoFailurePerPacket": None,
            },
        }

        previous = prev.get(dnai)
        if previous is not None and prev_ts is not None:
            interval = now - prev_ts
            delta = {}
            reset = False
            # Per-reason error deltas, computed the same way as the scalars.
            # A reason absent from a sample means the counter is still zero -
            # VPP omits zero rows from `show errors` entirely.
            prev_errs = previous.get("txErrors", {})
            err_delta = {}
            for reason, value in raw.get("txErrors", {}).items():
                before = prev_errs.get(reason, 0)
                if value < before:
                    reset = True
                    break
                err_delta[reason] = value - before
            if err_delta and not reset:
                delta["txErrors"] = err_delta
            for key, value in raw.items():
                if key in _VPP_GAUGES or key == "txErrors":
                    continue
                before = previous.get(key)
                if before is None:
                    continue
                if value < before:
                    # Counters only decrease when VPP restarted. Publishing a
                    # negative delta would look like negative traffic, so the
                    # whole sample is discarded instead.
                    reset = True
                    break
                delta[key] = value - before
            if reset:
                log.warning("VPP counters for %s went backwards - treating as a "
                            "restart and skipping this delta", interface)
            elif interval > 0:
                delta["intervalSec"] = round(interval, 3)
                entry["delta"] = delta
                entry["health"] = path_health(delta)

        fresh[dnai] = raw
        out[dnai] = entry

    _vpp_prev["ts"] = now
    _vpp_prev["counters"] = fresh
    return out


def derive_smf_features(smf_collection, window_sec):
    now = int(time.time())
    window_start = now - window_sec
    # ACTIVE sessions, not "SUPIs that ever had one".
    #
    # This used to be count_documents({"pdusesestlist.0": {"$exists": True}}),
    # an ALL-TIME count over the whole collection. It never decremented, because
    # PDU session releases are never recorded at all - oai-nwdaf-sbi's
    # getUpdatePDU_SES_REL() returns an error rather than an update. So it read
    # "1" whether or not any session existed, including four days after the SMF
    # stopped reporting entirely. It was also the ONLY non-zero feature reaching
    # the traffic-steering model once the SMF data went stale.
    #
    # A session that is up emits PFCP usage reports on a timer whether or not it
    # carries traffic, so "distinct SUPIs with a usage report inside the window"
    # is a live, honest measure of concurrent sessions.
    #
    # THE WINDOW MUST EXCEED THE SMF'S REPORTING PERIOD. The poller runs every
    # POLL_INTERVAL_SEC (5 s) but this SMF reports every ~10 s, so using the
    # poll interval as the window made the count alternate 1,0,1,0 - measured
    # exactly that before this was widened. ACTIVE_SESSION_WINDOW_SEC must stay
    # comfortably above the reporting period; 30 s tolerates one missed report.
    #
    # It is deliberately NOT used for the rate aggregation below: rates are
    # divided by each report's own `duration`, so widening their window would
    # sum several reporting periods and inflate them.
    #
    # CLASSIFICATION: OAI implementation gap (project telemetry defect).
    session_window_start = now - max(window_sec, ACTIVE_SESSION_WINDOW_SEC)
    active_pdu_sessions = len(smf_collection.distinct(
        "_id", {"qosmonlist.timestamp": {"$gte": session_window_start}}
    ))
    pipeline = [
        {"$unwind": "$qosmonlist"},
        {"$match": {"qosmonlist.timestamp": {"$gte": window_start}}},
        # De-duplicate before summing. Every oai-nwdaf-sbi restart registers a
        # NEW event subscription with the SMF without deleting the old one, so
        # SMF then delivers each notification once per stale subscription and
        # the identical report gets stored N times (confirmed live: same supi +
        # seid + urseqn + byte count, duplicated - inflating rates exactly 2x
        # after one extra restart). (seid, urseqn) is the usage report's own
        # unique identity, so grouping on it collapses duplicates regardless of
        # how many stale subscriptions exist.
        {
            "$group": {
                "_id": {
                    "supi": "$_id",
                    "seid": "$qosmonlist.customized_data.usagereport.seid",
                    "urseqn": "$qosmonlist.customized_data.usagereport.urseqn",
                },
                "volUl": {"$first": "$qosmonlist.customized_data.usagereport.volume.uplink"},
                "volDl": {"$first": "$qosmonlist.customized_data.usagereport.volume.downlink"},
                "pktUl": {"$first": "$qosmonlist.customized_data.usagereport.nop.uplink"},
                "pktDl": {"$first": "$qosmonlist.customized_data.usagereport.nop.downlink"},
                "dur": {"$first": "$qosmonlist.customized_data.usagereport.duration"},
            }
        },
        {
            "$group": {
                "_id": None,
                "ul": {"$sum": "$volUl"},
                "dl": {"$sum": "$volDl"},
                # PFCP usage reports also carry packet counts (nop) alongside byte
                # volumes. A UPF is often packets-per-second bound rather than
                # bits-per-second bound (per-packet forwarding cost dominates), so
                # pps is an independent congestion signal from bps - and the ratio
                # of the two gives average packet size, which separates bulk
                # transfer from small-packet floods at identical throughput.
                "ulPkts": {"$sum": "$pktUl"},
                "dlPkts": {"$sum": "$pktDl"},
                # Each usage report states the period it covers. Rates must be
                # divided by THAT, not by this poller's window: SMF emits reports
                # every ~10s each covering 10s of traffic, so dividing a 10s
                # accumulation by a 5s poll window inflated every rate ~2x
                # (for example, an iperf 534 Mbit/s load reported as 1124 Mbit/s).
                # Sessions report in parallel over the same wall-clock period, so
                # the divisor is one report's duration - never the sum of them.
                "reportDur": {"$max": "$dur"},
            }
        },
    ]
    agg = list(smf_collection.aggregate(pipeline))
    ul_bytes = agg[0]["ul"] if agg else 0
    dl_bytes = agg[0]["dl"] if agg else 0
    ul_pkts = agg[0]["ulPkts"] if agg else 0
    dl_pkts = agg[0]["dlPkts"] if agg else 0
    # Fall back to the poll window only if reports carry no usable duration.
    report_dur = (agg[0].get("reportDur") if agg else None) or window_sec
    return {
        "activePduSessions": active_pdu_sessions,
        "aggUlBps": (ul_bytes * 8) / report_dur,
        "aggDlBps": (dl_bytes * 8) / report_dur,
        "aggUlPps": ul_pkts / report_dur,
        "aggDlPps": dl_pkts / report_dur,
        # Unaffected by the divisor (ratio of two equally-scaled values) - this is
        # why avgPktSize read a correct 1500 B even while the rates were inflated.
        # 0 when no packets in window: read as "no data", not "zero-length packets".
        "avgPktSizeUl": (ul_bytes / ul_pkts) if ul_pkts else 0.0,
        "avgPktSizeDl": (dl_bytes / dl_pkts) if dl_pkts else 0.0,
    }


def poll_once(smf_collection, upf_metrics_collection):
    pid = get_container_pid(UPF_CONTAINER_NAME)
    cgroup_path = get_cgroup_path(pid)
    cpu_stat = read_cpu_stat(cgroup_path)
    memory_current = read_memory_current(cgroup_path)
    derived = derive_smf_features(smf_collection, POLL_INTERVAL_SEC)

    doc = {
        "upfId": UPF_ID,
        "timestamp": int(time.time()),
        "cpu": {
            "usageUsec": cpu_stat.get("usage_usec", 0),
            "userUsec": cpu_stat.get("user_usec", 0),
            "systemUsec": cpu_stat.get("system_usec", 0),
        },
        "memory": {"currentBytes": memory_current},
        "derived": derived,
    }

    # Additive and never fatal: a VPP CLI failure must not stop NF_LOAD data.
    dnai_counters = {}
    if COLLECT_VPP_COUNTERS:
        try:
            dnai_counters = collect_vpp_dnai_counters()
        except Exception as exc:
            log.warning("per-DNAI VPP counters unavailable this poll: %s", exc)
    if dnai_counters:
        doc["dnaiPerf"] = dnai_counters

    upf_metrics_collection.insert_one(doc)
    log.info(
        "upf=%s cpuUsageUsec=%d memBytes=%d activePduSessions=%d "
        "aggUlBps=%.0f aggDlBps=%.0f aggUlPps=%.0f aggDlPps=%.0f avgPktUl=%.0f",
        UPF_ID,
        doc["cpu"]["usageUsec"],
        memory_current,
        derived["activePduSessions"],
        derived["aggUlBps"],
        derived["aggDlBps"],
        derived["aggUlPps"],
        derived["aggDlPps"],
        derived["avgPktSizeUl"],
    )
    for dnai, entry in sorted(dnai_counters.items()):
        delta = entry.get("delta") or {}
        interval = delta.get("intervalSec")
        if interval:
            log.info(
                "  dnai=%-20s %-11s txBps=%-12.0f rxBps=%-12.0f "
                "txPps=%-9.0f drops+=%-4d sendtoFail+=%-5d vec/call=%-5.2f %s",
                dnai, entry["interface"],
                delta.get("tx bytes", 0) * 8 / interval,
                delta.get("rx bytes", 0) * 8 / interval,
                delta.get("tx packets", 0) / interval,
                delta.get("drops", 0),
                delta.get("txErrors", {}).get(SENDTO_FAILURE_REASON, 0),
                entry["raw"].get("vectorsPerCall", 0.0),
                entry.get("health", {}).get("state", "?"),
            )
        else:
            log.info("  dnai=%-20s %-11s (first sample - totals only)",
                     dnai, entry["interface"])
    return doc


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--once",
        action="store_true",
        help="poll a single time and exit (for sanity checks)",
    )
    parser.add_argument(
        "--interval",
        type=int,
        default=POLL_INTERVAL_SEC,
        help="poll interval in seconds (default: %(default)s)",
    )
    args = parser.parse_args()

    client = MongoClient(MONGODB_URI)
    db = client[MONGODB_DATABASE_NAME]
    smf_collection = db[MONGODB_COLLECTION_SMF]
    upf_metrics_collection = db[MONGODB_COLLECTION_UPF_METRICS]
    upf_metrics_collection.create_index([("upfId", 1), ("timestamp", -1)])

    log.info(
        "starting UPF metrics poller: container=%s upfId=%s interval=%ss mongo=%s/%s",
        UPF_CONTAINER_NAME,
        UPF_ID,
        args.interval,
        MONGODB_URI,
        MONGODB_DATABASE_NAME,
    )

    if args.once:
        poll_once(smf_collection, upf_metrics_collection)
        return

    while True:
        try:
            poll_once(smf_collection, upf_metrics_collection)
        except Exception as exc:
            log.error("poll failed: %s", exc)
        time.sleep(args.interval)


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        sys.exit(0)
