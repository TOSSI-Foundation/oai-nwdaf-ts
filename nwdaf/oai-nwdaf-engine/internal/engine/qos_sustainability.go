/*
 * SPDX-License-Identifier: LicenseRef-CSSL-1.0
 */

/*
 * QOS_SUSTAINABILITY - TS 23.288 V17.12.0 clause 6.9.
 *
 * KNOWN NON-COMPLIANCE - stated here so no reader mistakes this for clause 6.9.
 * Table 6.9.2-1 sources this analytic from OAM: "RAN UE Throughput ... Average
 * UE bitrate in the cell ... per timeslot, per cell, per 5QI and per S-NSSAI"
 * and "QoS flow Retainability". Table 6.9.3-1 defines the STATISTICS output as
 * Applicable Area (TAIs or Cell IDs), Applicable Time Period and Crossed
 * Reporting Threshold(s). Clause 6.9.1 makes QoS requirements (5QI) and
 * Location information MANDATORY filters.
 *
 * What this handler actually computes is the aggregate CORE-side user-plane
 * throughput over a time window, derived from PFCP usage reports proxied
 * through Nsmf_EventExposure into smf.qosmonlist. It has no cell, no 5QI, no
 * Applicable Area and no threshold-crossing evaluation, and the mandatory
 * filters are ignored. It is reported in the standard RanUeThrouThd attribute
 * because that is the only throughput-shaped field in the standard
 * QosSustainabilityInfo model - NOT because the value means what Table 6.9.2-1
 * says RAN UE Throughput means.
 *
 * CLASSIFICATION: Known non-compliance (semantically incorrect analytics ID).
 * Retained because the throughput computation itself is correct and reusable,
 * and because removing an already-exposed analytic would be a larger change
 * than labelling it. The correct home for a core-side per-path throughput
 * figure is DN_PERFORMANCE (clause 6.14) - a later phase.
 */

package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// ------------------------------------------------------------------------------
// qosSustainability - report observed QoS (throughput) over a time window.
func qosSustainability(w http.ResponseWriter, r *http.Request) {
	switch r.Method {

	case "GET":
		log.Printf("Getting Qos Sustainability from DB")
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
		applyDefaultRecentWindow(&engineReqData, 300)
		// re-uses the same qosmonlist filter as UE_COMMUNICATION
		filter := GetFilterUeComm(engineReqData)
		db := mongoClient.Database(config.Database.DbName)
		collection := db.Collection(config.Database.CollectionSmfName)
		cursor, err := collection.Find(context.Background(), filter)
		if err != nil {
			http.Error(w, "Error finding documents", http.StatusInternalServerError)
			return
		}
		startTs, endTs := getExtraReportReq(engineReqData)
		timeStamp := calculateTimeStamp(startTs, endTs)
		var totalVolBytes int64
		var totalDurSec int32
		sampleCount := 0
		// See usage_report_dedup.go.
		seen := make(map[usageReportKey]bool)
		for cursor.Next(context.Background()) {
			var result bson.M
			err := cursor.Decode(&result)
			if err != nil {
				http.Error(w, "Error decoding document", http.StatusInternalServerError)
				return
			}
			qosMonList, ok := result["qosmonlist"].(primitive.A)
			if !ok {
				continue
			}
			for _, qosMonElem := range qosMonList {
				qosMonMap, ok := qosMonElem.(bson.M)
				if !ok {
					continue
				}
				qosTimestamp := qosMonMap["timestamp"].(int64)
				if matchTimeStamp(qosTimestamp, timeStamp, startTs, endTs) {
					usageReport := qosMonMap["customized_data"].(bson.M)["usagereport"].(bson.M)
					if key, ok := usageReportKeyOf(usageReport); ok {
						if seen[key] {
							continue
						}
						seen[key] = true
					}
					volume := usageReport["volume"].(bson.M)
					duration := usageReport["duration"].(int32)
					totalVolBytes += volume["total"].(int64)
					totalDurSec += duration
					sampleCount++
				}
			}
		}
		// QosSustainabilityInfo's schema requires oneOf(qosFlowRetThd, ranUeThrouThd) -
		// we don't compute flow retainability, so ranUeThrouThd must always be set
		// (as "0 bps" when there's no usage data) or a no-data response violates
		// the schema by satisfying neither.
		var bps float64
		if totalDurSec > 0 {
			bps = float64(totalVolBytes*8) / float64(totalDurSec)
		}
		qosResp := QosSustainabilityResp{RanUeThrouThd: formatBitRate(bps)}
		// NO Confidence is emitted. TS 23.288 Table 6.9.3-1 ("QoS Sustainability"
		// STATISTICS, which is what this handler produces) defines only
		// Applicable Area, Applicable Time Period and Crossed Reporting
		// Threshold(s); Confidence appears solely in Table 6.9.3-2
		// (predictions). What was previously sent - min(sampleCount*5, 100) -
		// was a sample count presented as a confidence.
		_ = sampleCount
		w.Header().Set("Content-Type", "application/json")
		jsonResp, err := json.Marshal(qosResp)
		if err != nil {
			http.Error(w, "Error marshaling JSON", http.StatusInternalServerError)
			return
		}
		w.Write(jsonResp)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// ------------------------------------------------------------------------------
// formatBitRate - Format a bps value using the SI-prefix bit rate string format
// used by QosSustainabilityInfo.RanUeThrouThd (e.g. "512 Kbps").
func formatBitRate(bps float64) string {
	switch {
	case bps >= 1e9:
		return fmt.Sprintf("%.1f Gbps", bps/1e9)
	case bps >= 1e6:
		return fmt.Sprintf("%.1f Mbps", bps/1e6)
	case bps >= 1e3:
		return fmt.Sprintf("%.1f Kbps", bps/1e3)
	default:
		return fmt.Sprintf("%.0f bps", bps)
	}
}
