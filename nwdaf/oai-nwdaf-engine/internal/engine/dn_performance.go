/*
 * SPDX-License-Identifier: LicenseRef-CSSL-1.0
 */

/*
 * DN_PERFORMANCE - TS 23.288 V17.12.0 clause 6.14 "DN Performance Analytics".
 *
 * WHY THIS ANALYTIC EXISTS HERE
 * -----------------------------------------------------------------------------
 * NF_LOAD (clause 6.5) is a property of the NF INSTANCE. Both N6 DNAIs in this
 * deployment are network instances of the SAME UPF NF instance, so NF_LOAD
 * structurally cannot rank one against the other - measured, a sustained
 * 309 Mbit/s moved nfLoadLevelAverage from 6 to 6. DN_PERFORMANCE is the only
 * standard analytic whose OUTPUT IS PER-PATH: Table 6.14.3-1 carries
 * "> Serving anchor UPF info" and "> DNAI" per entry, and clause 6.14.4 names
 * the SMF as the consumer -
 *
 *   "If the analytics consumer is an SMF, the SMF may use the analytics to
 *    determine the UPF and DNAI that offers the best user plane performance."
 *
 * WHAT THIS PRODUCES, PRECISELY
 * -----------------------------------------------------------------------------
 * DN performance STATISTICS as defined in Table 6.14.3-1. NOT predictions:
 * Table 6.14.3-2 is not produced, and consequently NO Confidence attribute is
 * emitted - Table 6.14.3-1 has no such field. Same rule that kept Confidence
 * out of NF_LOAD and QOS_SUSTAINABILITY.
 *
 * Table 6.14.3-1 rows and where each comes from here:
 *   Application ID           -> NOT PRODUCED. No per-application traffic
 *                               classification exists anywhere in this
 *                               deployment. It is left ABSENT rather than
 *                               echoed back from the request filter, which
 *                               would assert that the measurement is
 *                               application-scoped when it is not.
 *   S-NSSAI, DNN             -> smf.pdusesestlist
 *   > Serving anchor UPF info-> NRF (clause 6.2.2.4), nrf_upf_identity.go
 *   > DNAI                   -> smf.uppathchlist, i.e. TS 29.508 UP_PATH_CH
 *   >> Average Traffic rate  -> PFCP usage reports: Sum(bits) / Sum(duration)
 *   >> Maximum Traffic rate  -> max single-report rate in the window
 *   >> Average Packet Delay  -> NOT PRODUCED, see below
 *   >> Maximum Packet Delay  -> NOT PRODUCED, see below
 *   >> Average Packet Loss Rate -> NOT PRODUCED, see below
 *   > Spatial Validity Condition -> NOT PRODUCED, no per-area scoping exists
 *   > Temporal Validity Condition-> the analytics target period actually used
 *
 * WHY THE DELAY AND LOSS FIELDS ARE ABSENT AND NOT ZERO
 * -----------------------------------------------------------------------------
 * The SMF returns null for ulDelays/dlDelays/rtDelays even under 588 Mbit/s,
 * and PFCP usage reports carry volume, packet counts and duration but NO loss
 * counter. Table 6.4.2-2 NOTE 1 is explicit that this is not an OAI defect:
 * "How NWDAF collects QoS flow Bit Rate, QoS flow Packet Delay, Packet
 * transmission and Packet retransmission information from UPF is not defined
 * in this Release of the specification."
 * CLASSIFICATION: 3GPP-standard behaviour (undefined collection in Rel-17).
 * An unmeasurable field is ABSENT. Zero would mean "measured zero".
 *
 * HONEST LIMITATION - DO NOT SOFTEN, DO NOT DESCRIBE THIS AS PER-DNAI
 * COMPARABILITY
 * -----------------------------------------------------------------------------
 * With ONE UPF and ONE active path, this can only measure the DNAI a session is
 * ACTUALLY ON. You cannot measure a path you are not using. What it gives is:
 *   - a real FEEDBACK SIGNAL: did throughput improve after the steer?
 *   - a HISTORICAL per-DNAI comparison.
 * What it is NOT is a COUNTERFACTUAL: two DNAIs' rates come from DIFFERENT time
 * windows that carried DIFFERENT offered load, so the comparison is not a
 * prediction of how the unused path would perform. A true counterfactual needs
 * concurrently active paths (multi-UPF) - a topology change, not code.
 *
 * KNOWN ATTRIBUTION LIMITATION - the join is PER SUPI, not per PDU session
 * -----------------------------------------------------------------------------
 * A usage report is attributed to a DNAI by TIME, within the SUPI's document.
 * It cannot be attributed per PDU session because QOS_MON notifications from
 * the OAI SMF carry pduSeId = 0 (observed across every stored report): the
 * QoS-monitoring path is keyed on the PFCP SEID and the SMF's own source says
 * "TODO: use SCID and access PDU Session ID (need binding SCIDs - PDUSessID)".
 * CLASSIFICATION: OAI implementation gap.
 * CONSEQUENCE: with more than one SIMULTANEOUS PDU session per UE on DIFFERENT
 * DNAIs, reports would be attributed to whichever DNAI the timeline says was
 * most recently set for that UE, which may be the wrong one. This deployment
 * runs one active PDU session per UE, where the time join is exact. This is
 * documented rather than silently mis-attributed.
 */

package engine

import (
	"context"
	"encoding/json"
	"io/ioutil"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// dnaiInterval - one entry of a session's DNAI timeline. The interval runs from
// start until the next entry's start, or until the end of the window for the
// last entry.
type dnaiInterval struct {
	dnai  string
	start int64
	// fromSource marks an interval derived from a notification's sourceDnai
	// ("the session WAS on this DNAI") rather than its targetDnai ("the session
	// IS now on this DNAI"). Only a source-derived earliest interval may be
	// extended backwards - see buildDnaiTimeline.
	fromSource bool
}

// dnPerfKey - the grouping identity of one Table 6.14.3-1 "DN performance"
// entry: the (DNN, S-NSSAI) that identifies the data network, plus the DNAI
// that identifies the path into it.
type dnPerfKey struct {
	dnn  string
	sst  int32
	sd   string
	dnai string
}

// dnPerfAccum - running aggregation for one dnPerfKey.
type dnPerfAccum struct {
	volBits    int64
	durSec     int64
	maxRateBps float64
	samples    int32
	firstTs    int64
	lastTs     int64
	// Per-report (timestamp, rate) points, kept ONLY so that a prediction can
	// fit a trend. Statistics never look at them.
	series []ratePoint
}

// ratePoint is one usage report expressed as a rate at an instant.
type ratePoint struct {
	ts  int64
	bps float64
}

// predictDnPerf - extrapolate a DNAI's traffic rate to `offsetSec` in the
// future, and say how much the series justifies believing it.
//
// TS 23.288 Table 6.14.3-2 defines DN Performance PREDICTIONS as the fields of
// Table 6.14.3-1 plus a Confidence. It does NOT define how either is computed;
// that is producer-specific. So both are defined here, explicitly, and both are
// PROJECT-SPECIFIC RESEARCH LOGIC - the Analytics ID, the output shape and the
// offsetPeriod request mechanism are the standard parts.
//
// THE PREDICTOR: ordinary least-squares straight line through the observed
// (time, rate) points, evaluated at now+offsetSec and clamped at zero. It is
// deliberately the simplest thing that can be explained in one sentence and
// audited from the logs. It is NOT a trained model, and it is unrelated to the
// XGBoost model behind the custom TRAFFIC_STEERING_UPF_LOAD analytic - the two
// tracks stay separate.
//
// THE CONFIDENCE, stated so nobody mistakes it for something it is not:
//   * it rises with the NUMBER of contributing reports, because two points can
//     be fitted exactly and mean nothing;
//   * it falls with the RELATIVE DISPERSION of the series, because a wildly
//     varying series does not support extrapolation;
//   * it is capped at 90 - this producer never claims near-certainty;
//   * it is NOT a calibrated probability, NOT validated against outcomes, and
//     NOT comparable with another NWDAF's confidence.
// Fewer than 3 points yields no prediction at all rather than a low-confidence
// one: absent, never faked.
// THE CEILING: a prediction is never allowed to exceed the highest rate the
// path has actually demonstrated. Straight-line extrapolation of a rising trend
// happily produces numbers the path has never once achieved - observed live, a
// predicted 1.20 Gbps against an observed maximum of 340 Mbps. Nothing in the
// series supports such a value, and a consumer ranking paths on it would be
// ranking on an artefact of the fit. Clamping to the observed maximum keeps the
// output inside what the data can justify.
// ------------------------------------------------------------------------------
// attributionIsAmbiguous - true when the per-SUPI, by-time attribution of usage
// reports to DNAIs could have put a report on the wrong path.
//
// Both conditions are necessary:
//   - more than one PFCP seid contributed, i.e. more than one PDU session's
//     reports are in the window for this SUPI; and
//   - more than one DNAI received reports, i.e. the timeline actually had a
//     choice to make.
//
// One seid is never ambiguous however many DNAIs the session moved between -
// that is a single session being steered, and the time join is exact for it.
// Several seids landing on ONE DNAI are not ambiguous either: whichever session
// a report came from, it lands on the same path. That second case is also the
// ordinary consequence of an SMF restart, which renumbers seids from 1 while
// the previous session's reports are still inside the window, and treating it
// as ambiguous would cry wolf on every redeploy.
func attributionIsAmbiguous(seids map[int64]bool, dnais map[string]bool) bool {
	return len(seids) > 1 && len(dnais) > 1
}

// ------------------------------------------------------------------------------
// dnPerformanceWindowSec - the default analytics window for DN_PERFORMANCE when
// the consumer supplies none, in seconds.
//
// This is the single largest term in the closed-loop reaction latency. A usage
// report changes the analytics output only once it falls inside this window,
// and the rate reported is an AVERAGE over the window, so a step change in
// offered load is diluted by the older samples still inside it and takes on the
// order of the window to become visible. Shortening the window speeds the loop
// up and costs statistical support: fewer samples for predictDnPerf() to fit,
// hence a lower Confidence, and below 3 samples no prediction at all.
//
// Returns the 300 s default for any non-positive configured value. That is
// deliberate: 0 must not be read as "no bound". An unbounded window is exactly
// the problem applyDefaultRecentWindow was introduced to fix (a 6.5 s scan of
// 248k accumulated entries), so a misconfiguration falls back to the safe value
// rather than reintroducing it.
func dnPerformanceWindowSec() int64 {
	if config.DnPerformance.WindowSec > 0 {
		return config.DnPerformance.WindowSec
	}
	return 300
}

func predictDnPerf(a *dnPerfAccum, offsetSec int32, now int64) (float64, int32, bool) {
	if a == nil || len(a.series) < 3 {
		return 0, 0, false
	}
	n := float64(len(a.series))
	var sumT, sumR, sumTT, sumTR float64
	for _, p := range a.series {
		t := float64(p.ts - a.firstTs) // seconds since the window start
		sumT += t
		sumR += p.bps
		sumTT += t * t
		sumTR += t * p.bps
	}
	mean := sumR / n
	denom := n*sumTT - sumT*sumT
	predicted := mean
	if denom != 0 {
		slope := (n*sumTR - sumT*sumR) / denom
		intercept := (sumR - slope*sumT) / n
		at := float64(now+int64(offsetSec)) - float64(a.firstTs)
		predicted = intercept + slope*at
	}
	if predicted < 0 {
		predicted = 0
	}
	// Never predict above what this path has been seen to do (see above).
	if a.maxRateBps > 0 && predicted > a.maxRateBps {
		predicted = a.maxRateBps
	}

	// Relative dispersion (coefficient of variation) of the observed rates.
	var variance float64
	for _, p := range a.series {
		d := p.bps - mean
		variance += d * d
	}
	variance /= n
	cv := 0.0
	if mean > 0 {
		cv = math.Sqrt(variance) / mean
	}

	// Sample term: 3 points -> ~0.3, 10 -> ~0.7, 20+ -> ~0.85.
	sampleTerm := n / (n + 4.0)
	// Stability term: cv 0 -> 1.0, cv 1 -> 0.5, cv 3 -> 0.25.
	stabilityTerm := 1.0 / (1.0 + cv)
	conf := int32(90.0 * sampleTerm * stabilityTerm)
	if conf < 1 {
		conf = 1
	}
	return predicted, conf, true
}

// ------------------------------------------------------------------------------
// dnPerformance - DN performance statistics per (DNN, S-NSSAI, DNAI).
//
// An EMPTY list is the correct answer when the consumer's Analytics Filter
// designates something this NWDAF has no data for; the NBI turns that into the
// TS 29.520 "204 No Content" response. A DNAI with no usage reports in the
// window is OMITTED, never reported as 0 bps - absent means "no analytics",
// zero would mean "measured zero".
func dnPerformance(w http.ResponseWriter, r *http.Request) {
	switch r.Method {

	case "GET":
		log.Printf("Getting DN Performance from DB")
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Error reading request body", http.StatusInternalServerError)
			return
		}
		var engineReqData EngineReqData
		err = json.Unmarshal(body, &engineReqData)
		if err != nil {
			http.Error(w, "Error unmarshaling JSON", http.StatusBadRequest)
			return
		}

		// TS 23.288 Table 6.14.1-1 lists "Application ID" as a filter, but no
		// per-application traffic classification exists in this deployment.
		// Answering an application-scoped question with whole-DNAI traffic
		// would be wrong, so the request is refused with no analytics rather
		// than answered incorrectly. Same negative discipline as an unknown
		// DNAI filter.
		if len(engineReqData.AppIds) > 0 {
			log.Printf(
				"DN_PERFORMANCE: filter designates application(s) %v, but no "+
					"per-application traffic classification exists in this "+
					"deployment - returning no analytics rather than "+
					"whole-DNAI traffic under an application identity",
				engineReqData.AppIds)
			writeDnPerformanceResponse(w, []DnPerfResp{})
			return
		}

		// Keep the UPF NF Instance ID current: TS 23.288 clause 6.2.2.4 allows
		// discovery "on a periodic basis", and the OAI UPF changes its
		// nfInstanceId on every restart.
		refreshUpfIdentity()

		// TS 23.288 Table 6.14.1-1 "Anchor UPF info" used as an Analytics
		// Filter. Reuses the clause 6.5.1 matcher: an empty value is no filter,
		// a non-empty one must designate the UPF this NWDAF actually measures.
		if strings.TrimSpace(engineReqData.UpfId) != "" &&
			!nfInstanceFilterMatches([]string{engineReqData.UpfId}) {
			known, _ := upfInstanceIds()
			log.Printf(
				"DN_PERFORMANCE: filter designates anchor UPF %q; this NWDAF "+
					"has data only for %v (%s) - returning no analytics",
				engineReqData.UpfId, known, upfIdentitySource())
			writeDnPerformanceResponse(w, []DnPerfResp{})
			return
		}

		upfId := primaryUpfInstanceId()
		if upfId == "" {
			// Table 6.14.3-1 carries "Serving anchor UPF info" per entry.
			// Reporting a path's performance without saying which UPF anchors
			// it would let a consumer attribute it to the wrong NF.
			log.Printf(
				"DN_PERFORMANCE: no UPF NF Instance ID available (NRF " +
					"unreachable and NF_LOAD_UPF_NF_INSTANCE_ID unset) - " +
					"returning no analytics")
			writeDnPerformanceResponse(w, []DnPerfResp{})
			return
		}

		// The window that dominates reaction latency; ENGINE_DN_PERFORMANCE_WINDOW_SEC
		// overrides it, default 300 (see EngineConfig.DnPerformance.WindowSec).
		// windowSource is recorded BEFORE the default is applied: once
		// applyDefaultRecentWindow has run, a consumer-supplied window and a
		// defaulted one are indistinguishable, and logging the configured
		// default for a request that carried its own window would misreport
		// which window produced the numbers.
		windowSource := "default"
		if !engineReqData.StartTs.IsZero() || !engineReqData.EndTs.IsZero() {
			windowSource = "consumer"
		}
		applyDefaultRecentWindow(&engineReqData, dnPerformanceWindowSec())
		effectiveWindowSec := int64(
			engineReqData.EndTs.Sub(engineReqData.StartTs).Seconds())
		startTs, endTs := getExtraReportReq(engineReqData)
		timeStamp := calculateTimeStamp(startTs, endTs)
		// Same qosmonlist window filter the other usage-report handlers use.
		filter := GetFilterUeComm(engineReqData)
		db := mongoClient.Database(config.Database.DbName)
		collection := db.Collection(config.Database.CollectionSmfName)
		cursor, err := collection.Find(context.Background(), filter)
		if err != nil {
			http.Error(w, "Error finding documents", http.StatusInternalServerError)
			return
		}

		accum := make(map[dnPerfKey]*dnPerfAccum)
		// Reports whose DNAI cannot be determined, counted so the log can say
		// so instead of the data silently going missing.
		unattributed := 0

		for cursor.Next(context.Background()) {
			var result bson.M
			if err := cursor.Decode(&result); err != nil {
				http.Error(w, "Error decoding document", http.StatusInternalServerError)
				return
			}

			// The DNAI timeline for this SUPI. Built from EVERY UP_PATH_CH
			// entry at or before the end of the window - the interval that
			// covers the window usually OPENED before it.
			timeline := buildDnaiTimeline(
				result["uppathchlist"], startTs, endTs, timeStamp)
			if len(timeline) == 0 {
				// No UP_PATH_CH data for this UE. Its traffic cannot be
				// attributed to a path, so it contributes nothing. Not an
				// error: an SMF that does not emit UP_PATH_CH simply yields no
				// DN performance analytics.
				continue
			}

			dnn, sst, sd := sessionIdentity(result["pdusesestlist"])

			qosMonList, ok := result["qosmonlist"].(primitive.A)
			if !ok {
				continue
			}
			// See usage_report_dedup.go: stale SMF subscriptions still cause
			// the identical report to be stored N times.
			seen := make(map[usageReportKey]bool)
			// AMBIGUITY DETECTION for the 18.7 attribution limitation.
			// A usage report carries no PDU Session ID - the OAI SMF sends
			// pduSeId = 0 on every QOS_MON notification - so reports are
			// attributed to a DNAI by TIME, within the SUPI's document. That is
			// exact while a SUPI has one PDU session, and ambiguous the moment
			// it has two on different DNAIs at once.
			//
			// The reports do carry the PFCP `seid`, which identifies the
			// session on the UPF, so the AMBIGUOUS CASE IS DETECTABLE even
			// though it is not resolvable: more than one seid contributing to
			// more than one DNAI inside the window means the time join has had
			// to choose. Detected and logged rather than left silent - the
			// output is unchanged, because the engine cannot tell which
			// attribution was right, and inventing one would be worse than
			// saying the reading is ambiguous.
			seidsSeen := make(map[int64]bool)
			dnaisAssigned := make(map[string]bool)
			for _, qosMonElem := range qosMonList {
				qosMonMap, ok := qosMonElem.(bson.M)
				if !ok {
					continue
				}
				qosTimestamp, ok := bsonInt64(qosMonMap["timestamp"])
				if !ok || !matchTimeStamp(qosTimestamp, timeStamp, startTs, endTs) {
					continue
				}
				usageReport, ok := usageReportOf(qosMonMap)
				if !ok {
					continue
				}
				if key, ok := usageReportKeyOf(usageReport); ok {
					if seen[key] {
						continue
					}
					seen[key] = true
				}
				volume, ok := usageReport["volume"].(bson.M)
				if !ok {
					continue
				}
				totalBytes, ok := bsonInt64(volume["total"])
				if !ok {
					continue
				}
				durationSec, ok := bsonInt64(usageReport["duration"])
				if !ok || durationSec <= 0 {
					// A report with no duration yields no rate. Skipping it is
					// correct: dividing by the poll window instead would
					// inflate the rate (the 2x bug recorded in the design).
					continue
				}

				dnai := dnaiAt(timeline, qosTimestamp)
				if dnai == "" {
					unattributed++
					continue
				}

				if seid, ok := bsonInt64(usageReport["seid"]); ok {
					seidsSeen[seid] = true
				}
				dnaisAssigned[dnai] = true

				key := dnPerfKey{dnn: dnn, sst: sst, sd: sd, dnai: dnai}
				a := accum[key]
				if a == nil {
					a = &dnPerfAccum{firstTs: qosTimestamp, lastTs: qosTimestamp}
					accum[key] = a
				}
				bits := totalBytes * 8
				a.volBits += bits
				a.durSec += durationSec
				a.samples++
				// Maximum Traffic rate uses the REPORT'S OWN duration.
				rate := float64(bits) / float64(durationSec)
				if rate > a.maxRateBps {
					a.maxRateBps = rate
				}
				a.series = append(a.series, ratePoint{ts: qosTimestamp, bps: rate})
				if qosTimestamp < a.firstTs {
					a.firstTs = qosTimestamp
				}
				if qosTimestamp > a.lastTs {
					a.lastTs = qosTimestamp
				}
			}

			// Both conditions are required. Several seids all landing on ONE
			// DNAI is unambiguous however many there are - every report goes to
			// the same path either way - and it is the ordinary case after an
			// SMF restart, which renumbers seids from 1 while the old session's
			// reports are still inside the window.
			if attributionIsAmbiguous(seidsSeen, dnaisAssigned) {
				log.Printf(
					"DN_PERFORMANCE: AMBIGUOUS ATTRIBUTION for %v - %d distinct "+
						"PFCP seids contributed to %d different DNAIs inside the "+
						"window, and QOS_MON carries pduSeId=0, so reports were "+
						"attributed by time alone and some may be on the wrong "+
						"path. Benign if these seids are "+
						"successive sessions of one UE; wrong if they are "+
						"concurrent.",
					result["_id"], len(seidsSeen), len(dnaisAssigned))
			}
		}

		if unattributed > 0 {
			log.Printf(
				"DN_PERFORMANCE: %d usage report(s) fell before the first "+
					"known DNAI of their UE and carried no sourceDnai - not "+
					"attributed to any path",
				unattributed)
		}

		resp := buildDnPerfResponse(accum, upfId, engineReqData)
		// Additive second source: per-DNAI interface health. Never replaces
		// PerfData and never fails the request when telemetry is missing.
		resp = attachDnaiPathHealth(resp, loadDnaiPathHealth(db))
		// windowSec is logged so a measurement can state which window produced
		// a given result instead of assuming the default was in force.
		log.Printf(
			"DN_PERFORMANCE: upfId=%s (%s) groups=%d windowSec=%d (%s)",
			upfId, upfIdentitySource(), len(resp), effectiveWindowSec,
			windowSource)
		for _, info := range resp {
			for _, p := range info.DnPerf {
				log.Printf(
					"DN_PERFORMANCE:   dnn=%s dnai=%s avg=%s max=%s samples=%d",
					info.Dnn, p.Dnai, p.PerfData.AvgTrafficRate,
					p.PerfData.MaxTrafficRate, p.SampleCount)
			}
		}
		writeDnPerformanceResponse(w, resp)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// ------------------------------------------------------------------------------
func writeDnPerformanceResponse(w http.ResponseWriter, list []DnPerfResp) {
	w.Header().Set("Content-Type", "application/json")
	jsonResp, err := json.Marshal(list)
	if err != nil {
		http.Error(w, "Error marshaling JSON", http.StatusInternalServerError)
		return
	}
	w.Write(jsonResp)
}

// ------------------------------------------------------------------------------
// usageReportOf - the customized_data.usagereport sub-document, without the
// chained unchecked type assertions the older handlers use (those panic on a
// malformed element and take the whole request with them).
func usageReportOf(qosMonMap bson.M) (bson.M, bool) {
	customized, ok := qosMonMap["customized_data"].(bson.M)
	if !ok {
		return nil, false
	}
	usageReport, ok := customized["usagereport"].(bson.M)
	if !ok {
		return nil, false
	}
	return usageReport, true
}

// ------------------------------------------------------------------------------
// buildDnaiTimeline - turn a SUPI's uppathchlist into a time-ordered list of
// DNAI intervals.
//
// Entries AFTER the end of the requested window are excluded: they describe a
// path the window never saw.
//
// The FIRST entry's sourceDnai, when present, opens an interval that covers the
// time BEFORE that entry - that is real information carried by TS 29.508
// (sourceDnai is "the DNAI the session was on"), not an assumption. When it is
// absent - which it genuinely is for a session's first notification - reports
// older than the first entry are left unattributed rather than guessed.
func buildDnaiTimeline(
	raw interface{}, startTs int64, endTs int64, timeStamp int64,
) []dnaiInterval {
	list, ok := raw.(primitive.A)
	if !ok {
		return nil
	}
	// Upper bound of the window, in the same two forms the other handlers use:
	// either an explicit endTs, or calculateTimeStamp's "before this instant".
	upper := endTs
	if timeStamp != 0 {
		upper = timeStamp
	}

	var intervals []dnaiInterval
	for _, elem := range list {
		m, ok := elem.(bson.M)
		if !ok {
			continue
		}
		ts, ok := bsonInt64(m["timestamp"])
		if !ok {
			continue
		}
		if upper > 0 && ts > upper {
			continue
		}
		target, _ := m["targetdnai"].(string)
		if target == "" {
			continue
		}
		source, _ := m["sourcednai"].(string)
		intervals = append(intervals, dnaiInterval{dnai: target, start: ts})
		if source != "" {
			// Opens one instant before, so that a report landing exactly on the
			// change timestamp is attributed to the TARGET, not the source.
			intervals = append(intervals, dnaiInterval{
				dnai: source, start: ts - 1, fromSource: true})
		}
	}
	if len(intervals) == 0 {
		return nil
	}
	sort.SliceStable(intervals, func(i, j int) bool {
		return intervals[i].start < intervals[j].start
	})
	// If the earliest thing known about this UE is a sourceDnai, TS 29.508 has
	// told us the session WAS on that DNAI before the change - so that interval
	// legitimately covers the earlier part of the requested window. This
	// recovers reports from a session whose establishment notification was
	// never seen (e.g. the SBI subscribed after the UE attached).
	//
	// It is extended back only to the START OF THE WINDOW, never indefinitely:
	// beyond the window there may have been an earlier session on a different
	// path, and claiming this DNAI for it would be an inference, not a
	// measurement. A targetDnai-derived earliest interval is NEVER extended -
	// it says where the session went, not where it had been.
	if intervals[0].fromSource && startTs > 0 && startTs < intervals[0].start {
		intervals[0].start = startTs
	}
	return intervals
}

// ------------------------------------------------------------------------------
// dnaiAt - the DNAI in effect at instant t, or "" when t precedes everything
// the timeline knows about.
func dnaiAt(timeline []dnaiInterval, t int64) string {
	dnai := ""
	for _, iv := range timeline {
		if iv.start > t {
			break
		}
		dnai = iv.dnai
	}
	return dnai
}

// ------------------------------------------------------------------------------
// sessionIdentity - the DNN and S-NSSAI of the SUPI's most recent PDU session
// establishment. Table 6.14.3-1 carries S-NSSAI and DNN per DN performance
// entry; both come from smf.pdusesestlist, which is where PDU_SES_EST stores
// them.
func sessionIdentity(raw interface{}) (string, int32, string) {
	list, ok := raw.(primitive.A)
	if !ok {
		return "", 0, ""
	}
	var bestTs int64 = -1
	dnn, sd := "", ""
	var sst int32
	for _, elem := range list {
		m, ok := elem.(bson.M)
		if !ok {
			continue
		}
		ts, _ := bsonInt64(m["timestamp"])
		if ts < bestTs {
			continue
		}
		bestTs = ts
		if v, ok := m["dnn"].(string); ok {
			dnn = v
		}
		if snssai, ok := m["snssai"].(bson.M); ok {
			if v, ok := bsonInt64(snssai["sst"]); ok {
				sst = int32(v)
			}
			if v, ok := snssai["sd"].(string); ok {
				sd = v
			}
		}
	}
	return dnn, sst, sd
}

// ------------------------------------------------------------------------------
// buildDnPerfResponse - turn the accumulator into Table 6.14.3-1 shaped output,
// applying the remaining Table 6.14.1-1 Analytics Filters (DNAI, DNN, S-NSSAI).
//
// Grouping follows the table's nesting: one entry per (Application ID, S-NSSAI,
// DNN) carrying a list of per-path "DN performance" records. Application ID is
// never set - see the file header.
// ------------------------------------------------------------------------------
// Per-DNAI path health from UPF N6 interface counters.
//
// THIS IS A SECOND, INDEPENDENT DATA SOURCE. Everything else in this file is
// built from PFCP usage reports joined to a DNAI through the per-SUPI
// `uppathchlist` timeline, which means a DNAI carrying no PDU session produces
// no usage report, gets no accumulator key, and is ABSENT from the response
// entirely - not zero, absent. Interface counters have no such dependency: they
// exist per N6 interface whether or not a session is on it, so they can answer
// "what is that path doing?" for a path nobody is using.
//
// The two sources are deliberately NOT merged into one number. PerfData keeps
// its usage-report meaning; path health is published beside it under a
// vendor-prefixed key.

// dnPerfHealthMaxAgeSec - how old a dnaiPerf sample may be and still be used.
//
// FRESHNESS WAS NOT PREVIOUSLY DEFINED FOR upf_metrics. `applyDefaultRecentWindow`
// (300 s, used by NF_LOAD) is an ANALYTICS WINDOW - the period to average over -
// not a staleness bound, and reusing it here would be wrong: the poller samples
// every 5 s, so a 300 s old health sample says nothing about the path now.
//
// The rule adopted, minimal and explicit: use only the newest sample, and if it
// is older than this bound report UNKNOWN_STALE rather than a stale verdict.
// Default 30 s = six poll intervals at the shipping 5 s cadence, so a couple of
// missed polls do not blank the signal but a dead collector does. Never use a
// sample of unknown age.
func dnPerfHealthMaxAgeSec() int64 {
	if config.DnPerformance.HealthMaxAgeSec > 0 {
		return config.DnPerformance.HealthMaxAgeSec
	}
	return 30
}

const (
	pathStateUnknownStale  = "UNKNOWN_STALE"
	pathStateUnknownNoData = "UNKNOWN_NO_DATA"
)

// loadDnaiPathHealth - {dnai: health} from the newest upf_metrics document.
//
// Returns an empty map (never an error) when telemetry is unavailable: path
// health is additive, and its absence must never fail a DN_PERFORMANCE request
// that the usage-report path can still answer.
func loadDnaiPathHealth(db *mongo.Database) map[string]*PathHealthResp {
	out := map[string]*PathHealthResp{}

	coll := db.Collection(config.Database.CollectionUpfMetricsName)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var doc bson.M
	err := coll.FindOne(
		ctx,
		bson.M{"dnaiPerf": bson.M{"$exists": true}},
		options.FindOne().SetSort(bson.D{{"timestamp", -1}}),
	).Decode(&doc)
	if err != nil {
		log.Printf("DN_PERFORMANCE: no per-DNAI path health available (%v)", err)
		return out
	}

	observedAt, _ := bsonInt64(doc["timestamp"])
	age := time.Now().Unix() - observedAt
	stale := age > dnPerfHealthMaxAgeSec()
	if stale {
		log.Printf(
			"DN_PERFORMANCE: newest dnaiPerf sample is %ds old (max %ds) - "+
				"reporting UNKNOWN_STALE rather than a stale verdict",
			age, dnPerfHealthMaxAgeSec())
	}

	perDnai, ok := doc["dnaiPerf"].(bson.M)
	if !ok {
		return out
	}
	for dnai, raw := range perDnai {
		entry, ok := raw.(bson.M)
		if !ok {
			continue
		}
		health, ok := entry["health"].(bson.M)
		if !ok {
			continue
		}
		state, _ := health["state"].(string)
		if state == "" {
			state = pathStateUnknownNoData
		}
		if stale {
			// The numbers may be real but they describe a moment that has
			// passed. Keep them for audit, replace the verdict.
			state = pathStateUnknownStale
		}
		ph := &PathHealthResp{
			State:       state,
			Denominator: asString(health["denominator"]),
			Semantics:   asString(health["semantics"]),
			ObservedAt:  observedAt,
			AgeSec:      age,
		}
		ph.TxAttempts, _ = bsonInt64(health["txAttempts"])
		ph.SendtoFailures, _ = bsonInt64(health["sendtoFailures"])
		// null stays null. A missing or non-numeric ratio is UNKNOWN, and
		// flattening it to 0 would read as "measured zero failures".
		if v, ok := bsonFloat64(health["sendtoFailurePerPacket"]); ok && !stale {
			ph.SendtoFailurePerPacket = &v
		}
		out[dnai] = ph
	}
	return out
}

// bsonFloat64 - a numeric BSON value as float64. Returns ok=false for nil and
// for anything non-numeric, so an ABSENT ratio stays absent instead of becoming
// a measured zero.
func bsonFloat64(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	}
	return 0, false
}

func asString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// attachDnaiPathHealth - hang path health off the response, and add entries for
// DNAIs the usage-report path could not see at all.
//
// A DNAI that HAS usage reports gets its health attached inline, so a consumer
// reading dnPerf sees traffic and health together. A DNAI with health but NO
// usage reports is emitted in a group carrying NO dnn and NO snssai - because
// interface health genuinely has neither. Inventing a DNN/S-NSSAI (or a SUPI)
// for it would assert an association the measurement does not have; the caller
// asked explicitly that this not be done.
func attachDnaiPathHealth(resp []DnPerfResp, health map[string]*PathHealthResp) []DnPerfResp {
	if len(health) == 0 {
		return resp
	}
	seen := map[string]bool{}
	for i := range resp {
		for j := range resp[i].DnPerf {
			dnai := resp[i].DnPerf[j].Dnai
			if h, ok := health[dnai]; ok {
				resp[i].DnPerf[j].PathHealth = h
				seen[dnai] = true
			}
		}
	}

	var orphans []DnPerfEntry
	for dnai, h := range health {
		if seen[dnai] {
			continue
		}
		// No PerfData: no usage report contributed, and an empty perfData is
		// the honest representation of "no traffic measurement", whereas a
		// zeroed rate would be read as a measured zero.
		orphans = append(orphans, DnPerfEntry{Dnai: dnai, PathHealth: h})
	}
	if len(orphans) == 0 {
		return resp
	}
	sort.Slice(orphans, func(a, b int) bool { return orphans[a].Dnai < orphans[b].Dnai })
	log.Printf(
		"DN_PERFORMANCE: %d DNAI(s) have path health but no usage reports - "+
			"emitted with no dnn/snssai, path-scoped only", len(orphans))
	return append(resp, DnPerfResp{DnPerf: orphans})
}

func buildDnPerfResponse(
	accum map[dnPerfKey]*dnPerfAccum,
	upfId string,
	engineReqData EngineReqData,
) []DnPerfResp {
	// TS 29.520: a POSITIVE offsetPeriod asks for a prediction that far ahead.
	// Anything else is the statistics path this engine has always served.
	predictAhead := engineReqData.OffsetPeriod
	predicting := predictAhead > 0
	nowTs := time.Now().Unix()
	type groupKey struct {
		dnn string
		sst int32
		sd  string
	}
	groups := make(map[groupKey][]DnPerfEntry)
	groupConfidence := make(map[groupKey]int32)

	// Deterministic output order: map iteration order is randomised in Go, and
	// a consumer ranking two DNAIs should not see them swap places between
	// identical requests.
	keys := make([]dnPerfKey, 0, len(accum))
	for k := range accum {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].dnn != keys[j].dnn {
			return keys[i].dnn < keys[j].dnn
		}
		if keys[i].sst != keys[j].sst {
			return keys[i].sst < keys[j].sst
		}
		if keys[i].sd != keys[j].sd {
			return keys[i].sd < keys[j].sd
		}
		return keys[i].dnai < keys[j].dnai
	})

	for _, k := range keys {
		a := accum[k]
		if a.durSec <= 0 || a.samples == 0 {
			continue
		}
		if !stringFilterMatches(engineReqData.Dnais, k.dnai) {
			continue
		}
		if !stringFilterMatches(engineReqData.Dnns, k.dnn) {
			continue
		}
		if !snssaiFilterMatches(engineReqData.Snssaia, k.sst, k.sd) {
			continue
		}

		avgBps := float64(a.volBits) / float64(a.durSec)
		var confidence int32
		predicted := false
		if predicting {
			p, c, ok := predictDnPerf(a, predictAhead, nowTs)
			if !ok {
				// Too few points to extrapolate from. A prediction is OMITTED
				// rather than emitted with a token confidence - the same
				// "absent, never faked" rule applied everywhere else here.
				log.Printf(
					"DN_PERFORMANCE: dnai=%s has %d sample(s), too few to "+
						"predict from - omitting it from the prediction",
					k.dnai, a.samples)
				continue
			}
			avgBps = p
			confidence = c
			predicted = true
		}

		entry := DnPerfEntry{
			UpfId: upfId,
			Dnai:  k.dnai,
			PerfData: PerfDataResp{
				// Statistics: Sum(bits) / Sum(duration), each report
				// contributing its own duration, never the poll window.
				// Predictions: the extrapolated rate from predictDnPerf().
				AvgTrafficRate: formatBitRate(avgBps),
				MaxTrafficRate: formatBitRate(a.maxRateBps),
				// avePacketDelay / maxPacketDelay / avgPacketLossRate are
				// deliberately not set - see the file header. omitempty keeps
				// them off the wire entirely.
			},
			Predicted: predicted,
			// Table 6.14.3-1 "> Temporal Validity Condition": the period the
			// reported figures were actually measured over, which is the span
			// of the contributing reports, not the requested window.
			TemporalValidCon: &TimeWindowResp{
				StartTime: time.Unix(a.firstTs, 0).UTC(),
				StopTime:  time.Unix(a.lastTs, 0).UTC(),
			},
			SampleCount: a.samples,
		}
		gk := groupKey{dnn: k.dnn, sst: k.sst, sd: k.sd}
		groups[gk] = append(groups[gk], entry)
		// Confidence lives on the DnPerfInfo grouping in the 3GPP model, not on
		// the per-path DnPerf. Keep the LOWEST confidence of the paths in a
		// group: a group is only as trustworthy as its weakest member.
		if predicted {
			if cur, seen := groupConfidence[gk]; !seen || confidence < cur {
				groupConfidence[gk] = confidence
			}
		}
	}

	groupKeys := make([]groupKey, 0, len(groups))
	for gk := range groups {
		groupKeys = append(groupKeys, gk)
	}
	sort.Slice(groupKeys, func(i, j int) bool {
		if groupKeys[i].dnn != groupKeys[j].dnn {
			return groupKeys[i].dnn < groupKeys[j].dnn
		}
		if groupKeys[i].sst != groupKeys[j].sst {
			return groupKeys[i].sst < groupKeys[j].sst
		}
		return groupKeys[i].sd < groupKeys[j].sd
	})

	out := make([]DnPerfResp, 0, len(groupKeys))
	for _, gk := range groupKeys {
		info := DnPerfResp{
			Dnn:    gk.dnn,
			DnPerf: groups[gk],
		}
		// Set ONLY for predictions. Statistics leave it zero and omitempty
		// keeps it off the wire (TS 23.288 Table 6.14.3-1 has no Confidence).
		if c, ok := groupConfidence[gk]; ok {
			info.Confidence = c
		}
		if gk.sst != 0 || gk.sd != "" {
			info.Snssai = &Snssai{Sst: gk.sst, Sd: gk.sd}
		}
		out = append(out, info)
	}
	return out
}

// ------------------------------------------------------------------------------
// stringFilterMatches - an empty filter list is "no filter" and matches
// everything; a non-empty one matches only its own members. Same semantics as
// nfInstanceFilterMatches, applied to the Table 6.14.1-1 DNAI and DNN filters.
func stringFilterMatches(filter []string, value string) bool {
	if len(filter) == 0 {
		return true
	}
	for _, want := range filter {
		if strings.EqualFold(strings.TrimSpace(want), value) {
			return true
		}
	}
	return false
}

// ------------------------------------------------------------------------------
// snssaiFilterMatches - Table 6.14.1-1 S-NSSAI filter. An SD is compared only
// when the filter carries one, because TS 23.003 makes SD optional.
func snssaiFilterMatches(filter []Snssai, sst int32, sd string) bool {
	if len(filter) == 0 {
		return true
	}
	for _, want := range filter {
		if want.Sst != sst {
			continue
		}
		if want.Sd == "" || strings.EqualFold(want.Sd, sd) {
			return true
		}
	}
	return false
}
