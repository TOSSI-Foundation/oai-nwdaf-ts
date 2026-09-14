/*
 * SPDX-License-Identifier: LicenseRef-CSSL-1.0
 */

/*
 * De-duplication of PFCP usage reports.
 *
 * WHY
 * -----------------------------------------------------------------------------
 * Every restart of oai-nwdaf-sbi used to add a NEW event-exposure subscription
 * to a still-running SMF without removing the previous one, so the SMF then
 * delivered each notification once per stale subscription and the IDENTICAL
 * usage report was stored N times. The duplicates carry the same supi + seid +
 * urseqn + byte count, inflating every summed rate exactly 2x after one extra
 * restart.
 *
 * scripts/telemetry/collect_upf_metrics.py already collapses duplicates in its
 * aggregation pipeline. The ENGINE handlers did not: ueComm and
 * qosSustainability walked qosmonlist in application code and added every
 * element, so UE_COMMUNICATION's volumes and QOS_SUSTAINABILITY's throughput
 * carried the same inflation.
 *
 * (seid, urseqn) is the usage report's own identity in PFCP: the F-SEID of the
 * session plus the UR-SEQN of the report (TS 29.244). Two stored elements with
 * the same pair are the same report, however many stale subscriptions produced
 * them.
 *
 * CLASSIFICATION: OAI implementation gap (mitigation). The ROOT CAUSE is that
 * neither the OAI SMF nor the OAI AMF implements the Individual Subscription
 * resource, so Nsmf/Namf_EventExposure_Unsubscribe cannot succeed - see the
 * header of oai-nwdaf-sbi/internal/sbi/subscription_manager.go. This is a
 * mitigation, not a fix, and must not be described as one.
 */

package engine

import "go.mongodb.org/mongo-driver/bson"

// usageReportKey identifies one PFCP usage report.
type usageReportKey struct {
	seid   int64
	urseqn int64
}

// usageReportKeyOf - extract (seid, urseqn) from a stored qosmonlist element.
// ok is false when the element does not carry a usage report, in which case the
// caller must fall back to counting it (dropping data would be worse than
// double-counting it).
func usageReportKeyOf(usageReport bson.M) (usageReportKey, bool) {
	seid, seidOk := bsonInt64(usageReport["seid"])
	urseqn, seqOk := bsonInt64(usageReport["urseqn"])
	if !seidOk || !seqOk {
		return usageReportKey{}, false
	}
	return usageReportKey{seid: seid, urseqn: urseqn}, true
}

// bsonInt64 - the driver decodes these as int32 or int64 depending on
// magnitude, so both have to be accepted.
func bsonInt64(v interface{}) (int64, bool) {
	switch n := v.(type) {
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		return int64(n), true
	}
	return 0, false
}
