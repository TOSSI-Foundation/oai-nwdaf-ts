// SPDX-License-Identifier: LicenseRef-CSSL-1.0

package analytics

// PathHealthExt - per-DNAI path health from UPF N6 interface counters.
//
// VENDOR EXTENSION. This is NOT defined by TS 23.288 or TS 29.520, and it is
// NOT any standard metric. It is deliberately carried under the vendor-prefixed
// key `oaiPathHealthExt`, OUTSIDE the 3GPP PerfData object, so that it can never
// be read as avgPacketLossRate / avePacketDelay / maxPacketDelay.
//
// It reports the fraction of transmit attempts on this DNAI's N6 interface that
// the UPF could not hand to the kernel socket - AF_PACKET transmit-side
// backpressure specific to this VPP-on-veth deployment. It counts stalls, not
// discarded packets: under every impairment measured, interface drops and Linux
// tx_dropped/tx_errors stayed at exactly zero.
//
// Unlike everything else in DnPerf, this is PATH-scoped rather than
// session-scoped: it comes from interface counters, not from PFCP usage
// reports, so it exists for a DNAI carrying no PDU session at all.
type PathHealthExt struct {
	// State is authoritative and must be read BEFORE the ratio.
	//
	//   OBSERVED_HEALTHY    enough transmit attempts, no stalls
	//   OBSERVED_DEGRADED   enough transmit attempts, stalls present
	//   UNKNOWN_NO_TRAFFIC  no transmit attempt - nothing could be observed
	//   UNKNOWN_INSUFFICIENT_SAMPLES
	//                       some traffic, but too little to tell healthy from
	//                       degraded. NOT healthy - "no failures in 3 packets"
	//                       is an empty window, not evidence.
	//   UNKNOWN_NO_SAMPLE   no interval to compare against yet
	//   UNKNOWN_STALE       telemetry too old to describe the path now
	//
	// UNKNOWN IS NOT HEALTHY. An idle path cannot stall, so an idle impaired
	// path is indistinguishable from an idle healthy one. A consumer that
	// treats "no failures" as "good" will select a path it knows nothing about.
	State string `json:"state"`

	// SendtoFailurePerPacket is a pointer so UNKNOWN serialises as JSON null.
	// It must never be flattened to 0.
	SendtoFailurePerPacket *float64 `json:"sendtoFailurePerPacket"`

	// Numerator, denominator and their description travel with the ratio so a
	// consumer can re-derive or re-normalise it instead of trusting this one.
	TxAttempts     int64  `json:"txAttempts"`
	SendtoFailures int64  `json:"sendtoFailures"`
	Denominator    string `json:"denominator,omitempty"`
	Semantics      string `json:"semantics,omitempty"`

	// ObservedAt/AgeSec make staleness auditable by the consumer.
	ObservedAt int64 `json:"observedAt,omitempty"`
	AgeSec     int64 `json:"ageSec"`
}
