# Networks, addresses and ports

Every address in this deployment is static and assigned in
[`compose/docker-compose-basic-vpp-pcf-steering.yaml`](../compose/docker-compose-basic-vpp-pcf-steering.yaml)
or in [`scripts/deploy/nwdaf_stack_up.sh`](../scripts/deploy/nwdaf_stack_up.sh).
Nothing uses DHCP: the NFs address each other by IP in `ulcl_config.yaml`, and
the UPF's N4 address is the one it registers in the NRF, so a moving address
breaks PFCP.

## Docker networks

| Network (docker name) | Bridge | Subnet | Role | Created by |
|---|---|---|---|---|
| `demo-oai-public-net` | `demo-oai` | `192.168.70.0/24` | SBI / control plane, N4, N2 | compose |
| `oai-public-access` | `cn5g-access` | `192.168.72.0/24` | N3, gNB ↔ UPF GTP-U | compose |
| `oai-public-core-pri` | `cn5g-core-pri` | `192.168.73.0/24` | **N6 primary** — DNAI `internet-primary` | compose |
| `oai-public-core-sec` | `cn5g-core-sec` | `192.168.74.0/24` | **N6 secondary** — DNAI `internet-secondary` | compose |
| `oai-nwdaf-net` | `cn5g-nwdaf` | `192.168.75.0/24` | NWDAF internal | `nwdaf_stack_up.sh` |

> **`192.168.75.0/24` is not the upstream default and must not be "corrected".**
> The NWDAF compose files shipped by OAI put `oai-nwdaf-net` on
> `192.168.74.0/24`, which is exactly this deployment's **secondary N6 network** —
> the path steering moves traffic onto. Creating it as shipped either fails or
> silently breaks steering. Override with `NWDAF_NET_PREFIX` if you must.

## Addresses

| Component | Network | IP | Host port | Purpose |
|---|---|---|---|---|
| `oai-nrf` | control | 192.168.70.130 | — | NF registration / discovery (HTTP/2, 8080) |
| `mysql` | control | 192.168.70.131 | — | subscriber database |
| `oai-amf` | control | 192.168.70.132 | — | N2/NGAP 38412/sctp, SBI 8080 |
| `oai-smf` | control | 192.168.70.133 | — | SBI 8080, PFCP 8805/udp, NWDAF consumer |
| `vpp-upf` | control | 192.168.70.134 | — | N4 signalling endpoint is **.201**, see below |
| `oai-udr` | control | 192.168.70.136 | — | |
| `oai-udm` | control | 192.168.70.137 | — | |
| `oai-ausf` | control | 192.168.70.138 | — | |
| `oai-pcf` | control | 192.168.70.139 | — | DNAI authorization |
| `gnbsim-vppN` | control | 192.168.70.14x | — | gNB control, N2 to the AMF |
| `oai-nwdaf-nbi-analytics` | control | 192.168.70.198 | 6059 | `Nnwdaf_AnalyticsInfo`, what the SMF polls |
| `oai-nwdaf-engine` | control | 192.168.70.196 | 6063 | NRF access for UPF NF-Instance-ID discovery |
| `oai-nwdaf-sbi` | control | 192.168.70.158 | 6062 | subscribes to AMF/SMF event exposure |
| `vpp-upf` | **N4** | **192.168.70.201** | — | the address the UPF registers in the NRF |
| `vpp-upf` | access | 192.168.72.134 / **.201** | — | N3 GTP-U |
| `gnbsim-vppN` | access | 192.168.72.14x | — | GTP-U local address |
| `vpp-upf` | core-pri | 192.168.73.134 / **.201** | — | N6 primary, veth `n6-3` |
| `oai-ext-dn` | core-pri | 192.168.73.135 | — | DN leg on the primary path |
| `vpp-upf` | core-sec | 192.168.74.134 / **.201** | — | N6 secondary, veth `n6-4` |
| `oai-ext-dn` | core-sec | 192.168.74.135 | — | DN leg on the secondary path |
| `oai-nwdaf-database` | nwdaf | 192.168.75.156 | 27017 | MongoDB |
| `oai-nwdaf-nbi-analytics` | nwdaf | 192.168.75.151 | 6059 | |
| `oai-nwdaf-nbi-events` | nwdaf | 192.168.75.152 | 6060 | |
| `oai-nwdaf-engine` | nwdaf | 192.168.75.155 | 6063 | |
| `oai-nwdaf-sbi` | nwdaf | 192.168.75.158 | 6062 | |
| `oai-nwdaf-engine-traffic-steering` | nwdaf | 192.168.75.159 | — | **optional**, ML forecast only |
| UEs | — | `12.1.1.0/24` | — | allocated by the SMF; `12.1.1.2` upward |

**`.201` is not a typo.** The VPP UPF binds a second address per interface for the
data path and for PFCP. `192.168.70.201` is what it puts in its NRF profile, and
Docker's embedded DNS cannot resolve it — which is why `recreate_smf.sh` passes
`--add-host vpp-upf.node.5gcn.mnc95.mcc208.3gppnetwork.org:192.168.70.201`.
Omit it and PFCP never associates; the symptom looks like a RAN failure.

## Ports published on the host

| Port | Service | Used by |
|---|---|---|
| 27017 | MongoDB | the host telemetry collector (`MONGODB_URI`) |
| 6059 | NWDAF analytics NBI | `scripts/test-rate.sh`, `start_nwdaf.sh` health check |
| 6060 | NWDAF events NBI | subscription clients |
| 6062 | NWDAF SBI | — |
| 6063 | NWDAF engine | — |

Check they are free before bringing the stack up:

```bash
ss -lntp | grep -E ':(27017|6059|6060|6062|6063)\b' || echo "all free"
```

## Which N6 veth is which DNAI

VPP names its host interfaces after the compose `IF_n` index, so the mapping is
authoritative from the UPF's own environment and must never be guessed:

```bash
sudo docker inspect vpp-upf --format '{{range .Config.Env}}{{println .}}{{end}}' \
  | grep -E '^IF_[0-9]+_(TYPE|NWI|DNAI)='
```

```
IF_3_TYPE=N6  IF_3_NWI=internet.oai.org.pri  IF_3_DNAI=internet-primary    → n6-3
IF_4_TYPE=N6  IF_4_NWI=internet.oai.org.sec  IF_4_DNAI=internet-secondary  → n6-4
```

`scripts/lib/upf_state.sh` derives it this way, so a re-addressed lab needs no edit.

## Re-addressing

The subnets are only hard-coded in two places — the compose file and
`nwdaf_stack_up.sh` — but the per-NF addresses also appear in
`configs/nf/ulcl_config.yaml` and in the `AMF_IP_ADDR` / `SMF_IP_ADDR` /
`NRF_URI` environment of the NWDAF containers. Changing a subnet means changing
all of them together. The NWDAF subnet is the one designed to move:
`NWDAF_NET_PREFIX=192.168.90 ./scripts/deploy/nwdaf_stack_up.sh`.
