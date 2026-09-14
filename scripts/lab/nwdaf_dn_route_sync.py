#!/usr/bin/env python3
# SPDX-License-Identifier: LicenseRef-CSSL-1.0
"""
Make the Data Network's return path follow the DNAI a session is actually on.

THE PROBLEM
-----------
`upf_graph::apply_steering_dnai()` rebinds two things on a steer: the uplink FAR
(egress network instance) and the downlink PDR's match instance. Nothing tells
the DN to send the reply back on the *other* N6 interface. `oai-ext-dn` carries
STATIC per-UE-IP routes, so after a steer the reply arrives on the old N6, is
looked up against a downlink PDR that now matches the new network instance,
matches nothing, and is dropped. Measured: 100% packet loss, both directions.

Before this script the demo only worked when the static route happened to agree
with where the SMF had put the session - by coincidence, not by design.

WHAT THIS DOES, AND WHY IT IS READ THIS WAY
-------------------------------------------
The source of truth is the UPF's own downlink PDR, read from
`vppctl show upf session`:

    PDR: 2
      PDI:
        Source Interface: Core                    <- downlink
        Network Instance: internet.oai.org.sec    <- what the UPF will MATCH on
        UE IP address (destination):
          IPv4 address: 12.1.1.3

That pair - (network instance, UE IP) - IS the rule return traffic has to
satisfy. Driving the DN route from it means the route can never disagree with
the dataplane, which is not true of driving it from the SMF's intent, from a log
line, or from `uppathchlist` (all of which lead the UPF and can be wrong while a
PFCP update is in flight, or after one has failed).

Network instance -> N6 next hop comes from the UPF container's own environment
(`IF_n_TYPE=N6`, `IF_n_NWI`, `IF_n_IP`), so the mapping is never hardcoded here
and stays correct if the topology is renumbered.

WHAT THIS IS NOT
----------------
This is a LAB SUBSTITUTE for DN routing, not a 3GPP procedure. In a real
deployment the PSA UPF would advertise the UE prefix out its active N6 interface
and the DN's routing protocol would converge on its own; TS 23.501 5.6.4 (ULCL /
Branching Point) is the standards-defined mechanism when two local exits are
genuinely different networks. Nothing here is claimed as standards-compliant.

It also does NOT change the UE address, and does not need to: because the route
is keyed on the UE IP actually read from the UPF, the address-pool drift
described above stops mattering. Fighting the allocator was
the wrong fix; making the return path address-agnostic is the right one.
"""

import argparse
import logging
import re
import subprocess
import sys
import time

VPPCTL = "/openair-upf/bin/vppctl"   # NOT on $PATH inside the container
UPF_CONTAINER = "vpp-upf"
DN_CONTAINER = "oai-ext-dn"


def run(cmd, timeout=15):
    """Run a command, returning stdout. Falls back to sudo -n like the other
    host-side tooling in this directory (collect_upf_metrics.py does the same)."""
    try:
        r = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
        if r.returncode != 0 and "permission denied" in (r.stderr or "").lower():
            r = subprocess.run(["sudo", "-n"] + cmd, capture_output=True,
                               text=True, timeout=timeout)
        if r.returncode != 0:
            raise RuntimeError((r.stderr or r.stdout or "").strip())
        return r.stdout
    except subprocess.TimeoutExpired:
        raise RuntimeError("timed out: %s" % " ".join(cmd))


def docker_exec(container, argv, timeout=15):
    return run(["docker", "exec", container] + argv, timeout=timeout)


def n6_map():
    """{network_instance: (next_hop_ip, dnai)} for every N6 interface, taken
    from the UPF container's own environment so nothing is hardcoded."""
    out = run(["docker", "inspect", UPF_CONTAINER,
               "--format", "{{range .Config.Env}}{{println .}}{{end}}"])
    env = {}
    for line in out.splitlines():
        if "=" in line:
            k, _, v = line.partition("=")
            env[k.strip()] = v.strip()

    mapping = {}
    for idx in range(1, 32):
        if env.get("IF_%d_TYPE" % idx) != "N6":
            continue
        nwi = env.get("IF_%d_NWI" % idx)
        ip = env.get("IF_%d_IP" % idx)
        dnai = env.get("IF_%d_DNAI" % idx, "?")
        if nwi and ip:
            mapping[nwi] = (ip, dnai)
    return mapping


_RE_IPV4 = re.compile(r"IPv4 address:\s*([0-9.]+)")


def downlink_bindings():
    """{ue_ipv4: network_instance} from every DOWNLINK PDR the UPF holds.

    A downlink PDR is 'Source Interface: Core' with a UE IP address
    (destination). Its Network Instance is what return traffic must arrive on.
    """
    text = docker_exec(UPF_CONTAINER, [VPPCTL, "show", "upf", "session"])
    out = {}
    src = nwi = ue = None
    want_ip = False

    for raw in text.splitlines():
        line = raw.strip()

        # A new session or a new rule ends whatever we were accumulating.
        if line.startswith("CP F-SEID:") or line.startswith("PDR:"):
            src = nwi = ue = None
            want_ip = False
            continue

        if line.startswith("Source Interface:"):
            src = line.split(":", 1)[1].strip()
        elif line.startswith("Network Instance:"):
            # Only the FIRST one after a PDR header belongs to that PDI; the
            # standalone 'FAR: n' blocks later in the dump also carry one, but
            # by then src is None (cleared at 'FAR Id:') so nothing records.
            if nwi is None:
                nwi = line.split(":", 1)[1].strip()
        elif line.startswith("UE IP address (destination)"):
            want_ip = True
        elif want_ip:
            m = _RE_IPV4.match(line)
            if m:
                ue = m.group(1)
                want_ip = False
        elif line.startswith("FAR Id:"):
            # End of this PDI. Record it if it was a usable downlink rule.
            if src == "Core" and ue and nwi:
                if ue in out and out[ue] != nwi:
                    logging.warning(
                        "UE %s appears on two network instances (%s and %s) - "
                        "using the later one; this should not happen with one "
                        "PDU session per UE",
                        ue, out[ue], nwi)
                out[ue] = nwi
            src = nwi = ue = None
            want_ip = False

    return out


_RE_ROUTE = re.compile(r"^([0-9.]+)(?:/32)?\s+via\s+([0-9.]+)")


def dn_routes():
    """{ue_ipv4: next_hop} for host routes currently in the DN container."""
    text = docker_exec(DN_CONTAINER, ["ip", "route"])
    out = {}
    for line in text.splitlines():
        m = _RE_ROUTE.match(line.strip())
        if m:
            out[m.group(1)] = m.group(2)
    return out


def sync_once(dry_run=False):
    """Returns the list of (ue, old_next_hop, new_next_hop, dnai) actually changed."""
    nwis = n6_map()
    if not nwis:
        logging.error("no N6 interfaces found in the %s environment", UPF_CONTAINER)
        return []

    bindings = downlink_bindings()
    if not bindings:
        logging.info("no downlink PDRs with a UE IP - nothing to sync")
        return []

    current = dn_routes()
    changed = []

    for ue, nwi in sorted(bindings.items()):
        target = nwis.get(nwi)
        if target is None:
            # An N3/access instance, or an instance the UPF does not expose on
            # N6. Never guess a next hop for it.
            logging.debug("UE %s is on '%s', which is not an N6 network "
                          "instance - leaving its route alone", ue, nwi)
            continue
        next_hop, dnai = target
        have = current.get(ue)
        if have == next_hop:
            continue

        logging.info("UE %s is on DNAI '%s' (%s) -> route via %s%s",
                     ue, dnai, nwi, next_hop,
                     "" if have is None else " (was %s)" % have)
        if not dry_run:
            docker_exec(DN_CONTAINER,
                        ["ip", "route", "replace", "%s/32" % ue, "via", next_hop])
        changed.append((ue, have, next_hop, dnai))

    return changed


def main():
    ap = argparse.ArgumentParser(
        description="Keep oai-ext-dn's UE return routes pointed at whichever N6 "
                    "network instance the UPF's downlink PDR currently matches.")
    ap.add_argument("--interval", type=float, default=0,
                    help="poll every N seconds; 0 (default) means run once and exit")
    ap.add_argument("--dry-run", action="store_true",
                    help="report what would change without touching any route")
    ap.add_argument("--verbose", "-v", action="store_true")
    args = ap.parse_args()

    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)s %(message)s")

    if args.dry_run:
        logging.info("DRY RUN - no route will be modified")

    if args.interval <= 0:
        changed = sync_once(dry_run=args.dry_run)
        logging.info("%d route(s) %s", len(changed),
                     "would change" if args.dry_run else "changed")
        return 0

    logging.info("watching every %.1fs (Ctrl-C to stop)", args.interval)
    while True:
        try:
            sync_once(dry_run=args.dry_run)
        except Exception as exc:                      # keep the watcher alive
            logging.warning("sync failed, will retry: %s", exc)
        time.sleep(args.interval)


if __name__ == "__main__":
    sys.exit(main())
