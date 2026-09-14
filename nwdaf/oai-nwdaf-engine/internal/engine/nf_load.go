/*
 * SPDX-License-Identifier: LicenseRef-CSSL-1.0
 */

/*
 * NF_LOAD - TS 23.288 V17.12.0 clause 6.5 "NF load analytics", scoped to the UPF.
 *
 * WHAT THIS PRODUCES, PRECISELY
 * -----------------------------------------------------------------------------
 * NF load STATISTICS as defined in Table 6.5.3-1, for ONE NF instance (the UPF
 * this NWDAF measures). Not predictions: Table 6.5.3-2 is not produced, and
 * consequently NO Confidence attribute is emitted - Confidence exists only in
 * the predictions table.
 *
 * Table 6.5.3-1 rows and where each comes from here:
 *   NF type            -> constant "UPF"
 *   NF instance ID     -> Nnrf_NFDiscovery (clause 6.2.2.4), see nrf_upf_identity.go
 *   NF status          -> NOT PRODUCED. Its source is the NRF NF profile
 *                         (Table 6.5.2-1 "NF status"), which this engine does
 *                         not yet read. KNOWN GAP, deliberately left absent
 *                         rather than filled with an invented value.
 *   NF resource usage  -> nfCpuUsage / nfMemoryUsage, from cgroup v2 accounting
 *   NF load            -> nfLoadLevelAverage
 *   NF peak load       -> nfLoadLevelpeak
 *   NF load per AoI    -> NOT PRODUCED. Table 6.5.3-1 NOTE 2 restricts it to AMF.
 *
 * INPUT DATA AND ITS STANDARDS STATUS
 * -----------------------------------------------------------------------------
 * Table 6.5.2-1 sources NF resource usage from OAM (mean virtual CPU/memory/disk
 * per TS 28.552 clause 5.7) and NF load/status from the NRF. This engine instead
 * reads cgroup v2 accounting for the UPF container, collected out of band by
 * scripts/telemetry/collect_upf_metrics.py.
 *
 * CLASSIFICATION: Deployment/test-environment workaround standing in for the OAM
 * source. It is NOT a specification violation in the narrow sense - TS 23.288
 * Table 6.2.2.1-1 NOTE 1 states that "NWDAF can collect some UPF input data for
 * deriving analytics, but how NWDAF collects these UPF input data is not defined
 * in this Release of the specification" - but it is also NOT the clause 6.2.3
 * OAM interface, and must not be described as one.
 *
 * KNOWN NON-COMPLIANCE / HONEST LIMITATION - DO NOT SOFTEN
 * -----------------------------------------------------------------------------
 * NF load is a property of the NF INSTANCE (Table 6.5.3-1). It is NOT path-aware
 * and NOT DNAI-aware. In this deployment both N6 DNAIs (internet-primary,
 * internet-secondary) are network instances of the SAME UPF NF instance, so this
 * analytic CANNOT rank one DNAI against the other, however well it is
 * implemented. Measured: a sustained 309 Mbit/s moved nfLoadLevelAverage from
 * 6 to 6. The path-scoped analytic is DN_PERFORMANCE (clause 6.14, output
 * Table 6.14.3-1 carries "Serving anchor UPF info" and "DNAI"); it is a
 * separate, later phase and is NOT implemented here.
 */

package engine

import (
	"context"
	"encoding/json"
	"io/ioutil"
	"log"
	"net/http"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// upfMetricSample mirrors one document written by collect_upf_metrics.py.
type upfMetricSample struct {
	UpfId     string `bson:"upfId"`
	Timestamp int64  `bson:"timestamp"`
	Cpu       struct {
		UsageUsec int64 `bson:"usageUsec"`
	} `bson:"cpu"`
	Memory struct {
		CurrentBytes int64 `bson:"currentBytes"`
	} `bson:"memory"`
}

// ------------------------------------------------------------------------------
// nfLoad - NF load statistics for the designated NF instance(s).
//
// The response is a LIST, mirroring Table 6.5.3-1's "List of resource status
// (1..max)" and clause 6.5.1's "the NWDAF shall provide the analytics for each
// designated NF instance". An EMPTY list is the correct answer when the
// consumer's Analytics Filter designates an NF this NWDAF has no data for -
// previously the filter was ignored entirely and this NWDAF's own reading was
// returned under whatever identity it had been configured with.
func nfLoad(w http.ResponseWriter, r *http.Request) {
	switch r.Method {

	case "GET":
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

		// Keep the NF Instance ID current before both the filter check and the
		// response: TS 23.288 clause 6.2.2.4 allows discovery "on a periodic
		// basis", and the OAI UPF changes its nfInstanceId on every restart.
		refreshUpfIdentity()

		// TS 23.288 clause 6.5.1 Analytics Filter Information.
		if !nfInstanceFilterMatches(engineReqData.NfInstanceIds) {
			known, _ := upfInstanceIds()
			log.Printf(
				"NF_LOAD: filter designates NF instance(s) %v; this NWDAF has "+
					"data only for %v (%s) - returning no analytics",
				engineReqData.NfInstanceIds, known, upfIdentitySource())
			writeNfLoadResponse(w, []NfLoadResp{})
			return
		}

		applyDefaultRecentWindow(&engineReqData, 300)
		startTs, endTs := getExtraReportReq(engineReqData)
		filter := getFilterNfLoad(startTs, endTs)
		db := mongoClient.Database(config.Database.DbName)
		collection := db.Collection(config.Database.CollectionUpfMetricsName)
		findOptions := options.Find().SetSort(bson.D{{"timestamp", 1}})
		cursor, err := collection.Find(context.Background(), filter, findOptions)
		if err != nil {
			http.Error(w, "Error finding documents", http.StatusInternalServerError)
			return
		}
		var samples []upfMetricSample
		if err := cursor.All(context.Background(), &samples); err != nil {
			http.Error(w, "Error decoding documents", http.StatusInternalServerError)
			return
		}

		instanceId := primaryUpfInstanceId()
		if instanceId == "" {
			// Table 6.5.3-1 requires an NF instance ID. Reporting load under no
			// identity at all would let a consumer attribute it to the wrong NF.
			log.Printf(
				"NF_LOAD: no NF Instance ID available (NRF unreachable and " +
					"NF_LOAD_UPF_NF_INSTANCE_ID unset) - returning no analytics")
			writeNfLoadResponse(w, []NfLoadResp{})
			return
		}
		resp := computeNfLoad(samples)
		resp.NfType = "UPF"
		resp.NfInstanceId = instanceId
		resp.IdentitySource = upfIdentitySource()
		log.Printf(
			"NF_LOAD: nfInstanceId=%s (%s) samples=%d avg=%d peak=%d cpu=%d mem=%d",
			resp.NfInstanceId, resp.IdentitySource, resp.SampleCount,
			resp.NfLoadLevelAverage, resp.NfLoadLevelpeak,
			resp.NfCpuUsage, resp.NfMemoryUsage)
		writeNfLoadResponse(w, []NfLoadResp{resp})

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// ------------------------------------------------------------------------------
func writeNfLoadResponse(w http.ResponseWriter, list []NfLoadResp) {
	w.Header().Set("Content-Type", "application/json")
	jsonResp, err := json.Marshal(list)
	if err != nil {
		http.Error(w, "Error marshaling JSON", http.StatusInternalServerError)
		return
	}
	w.Write(jsonResp)
}

// ------------------------------------------------------------------------------
// getFilterNfLoad - Get filter to retrieve upf_metrics samples for the configured UPF
// according to startTs and endTs values.
func getFilterNfLoad(startTs int64, endTs int64) bson.D {
	timeStampCondition := getTimeStampCondition(startTs, endTs)
	return bson.D{
		{"upfId", config.NfLoad.UpfId},
		{"timestamp", timeStampCondition},
	}
}

// ------------------------------------------------------------------------------
// computeNfLoad - Turn a time-ordered slice of cgroup samples into a load response.
// CPU is a cumulative counter (usec of CPU time consumed), so load % is derived from
// the delta between consecutive samples, normalized by NfLoad.UpfCpuCores (the UPF
// container has no cgroup CPU quota set, so "capacity" is a configured assumption,
// not something readable from cgroup itself). Memory is already a point-in-time gauge.
func computeNfLoad(samples []upfMetricSample) NfLoadResp {
	resp := NfLoadResp{}
	resp.SampleCount = int32(len(samples))
	if len(samples) == 0 {
		return resp
	}
	// Memory: gauge, average the raw percentages across all samples.
	var memPercentSum, memPercentPeak float64
	for _, s := range samples {
		p := percentOf(float64(s.Memory.CurrentBytes), float64(config.NfLoad.UpfMemCapacityBytes))
		memPercentSum += p
		if p > memPercentPeak {
			memPercentPeak = p
		}
	}
	resp.NfMemoryUsage = int32(clampPercent(memPercentSum / float64(len(samples))))

	if len(samples) < 2 {
		// A single sample cannot yield a CPU rate; report memory only.
		return resp
	}
	var cpuPercentSum float64
	var cpuPercentPeak float64
	intervals := 0
	for i := 1; i < len(samples); i++ {
		prev, cur := samples[i-1], samples[i]
		deltaWallSec := cur.Timestamp - prev.Timestamp
		if deltaWallSec <= 0 {
			continue
		}
		deltaUsageSec := float64(cur.Cpu.UsageUsec-prev.Cpu.UsageUsec) / 1e6
		cpuPercent := clampPercent(deltaUsageSec / (float64(deltaWallSec) * config.NfLoad.UpfCpuCores) * 100)
		cpuPercentSum += cpuPercent
		if cpuPercent > cpuPercentPeak {
			cpuPercentPeak = cpuPercent
		}
		intervals++
	}
	if intervals > 0 {
		resp.NfCpuUsage = int32(cpuPercentSum / float64(intervals))
	}
	resp.NfLoadLevelAverage = resp.NfCpuUsage
	resp.NfLoadLevelpeak = int32(cpuPercentPeak)
	return resp
}

func percentOf(value float64, capacity float64) float64 {
	if capacity <= 0 {
		return 0
	}
	return clampPercent(value / capacity * 100)
}

func clampPercent(p float64) float64 {
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}
