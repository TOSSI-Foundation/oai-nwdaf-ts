/*
 * SPDX-License-Identifier: LicenseRef-CSSL-1.0
 */

/*
 * This file contains functions related to Network Performance event ID.
 */

package engine

import (
	"context"
	"encoding/json"
	"io/ioutil"
	"log"
	"net/http"
	"strings"

	"go.mongodb.org/mongo-driver/bson"
)

// ------------------------------------------------------------------------------
// nwPerfNumOfUe - get the number of Ue.
func nwPerfNumOfUe(w http.ResponseWriter, r *http.Request) {
	switch r.Method {

	case "GET":
		log.Printf("Getting Number of UE Info from DB")
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
		// Create filter and calculate number of UEs
		filter := getFilterNwPerfNumUe(engineReqData)
		db := mongoClient.Database(config.Database.DbName)
		collection := db.Collection(config.Database.CollectionAmfName)
		log.Printf("Counting documents from mongo DB using filter ...")
		absoluteNum, err := collection.CountDocuments(context.Background(), filter)
		if err != nil {
			http.Error(w, "Error counting documents", http.StatusInternalServerError)
			return
		}
		//TODO - Implement relative ratio and confidence
		relativeRatio, confidence := int32(0), int32(0)
		// Prepare http response body
		nwPerfResp := NwPerfResp{
			RelativeRatio: relativeRatio,
			AbsoluteNum:   int32(absoluteNum),
			Confidence:    confidence,
		}
		w.Header().Set("Content-Type", "application/json")
		jsonResp, err := json.Marshal(nwPerfResp)
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
// nwPerfNumOfPdu - Get the number of PDU sessions.
func nwPerfNumOfPdu(w http.ResponseWriter, r *http.Request) {
	switch r.Method {

	case "GET":
		log.Printf("Getting Number of Pdu Sessions from DB")
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
		// get filter to calculate number of users in given network area
		filter := getFilterNwPerfNumPdu(engineReqData)
		db := mongoClient.Database(config.Database.DbName)
		collection := db.Collection(config.Database.CollectionSmfName)
		log.Printf("Counting documents from mongo DB using filter ...")
		absoluteNum, err := collection.CountDocuments(context.Background(), filter)
		if err != nil {
			http.Error(w, "Error counting documents", http.StatusInternalServerError)
			return
		}
		//Implement relative ratio and confidence
		relativeRatio, confidence := int32(0), int32(0)
		// prepare http response body
		nwPerfResp := NwPerfResp{
			RelativeRatio: relativeRatio,
			AbsoluteNum:   int32(absoluteNum),
			Confidence:    confidence,
		}
		w.Header().Set("Content-Type", "application/json")
		jsonResp, err := json.Marshal(nwPerfResp)
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
// getFilterNwPerfNumUe - Get request filter that will be used to calculates the number of UEs
func getFilterNwPerfNumUe(engineReqData EngineReqData) bson.D {
	log.Printf("Constructing filter for DB search ...")
	// get startTs and endTs
	startTs, endTs := getExtraReportReq(engineReqData)
	timeStampCondition := getTimeStampCondition(startTs, endTs)
	// get timestamps filter - UE that are registered at timestampCondition
	filterTimeReg := bson.D{
		{"rminfolist",
			bson.M{"$elemMatch": bson.M{"rminfo.rmstate.rmstateanyof": "REGISTERED",
				"timestamp": timeStampCondition}},
		}}
	// get Network Area filter
	filterLoc := bson.D{}
	if engineReqData.Tais != nil {
		tacs := make([]string, 0)
		plmnids := make([]PlmnId, 0)
		for _, t := range engineReqData.Tais {
			tacs = append(tacs, t.Tac)
			plmnids = append(plmnids, t.PlmnId)
		}
		// filter UE that are located in network area at timestampCondition
		filterLoc = bson.D{{"locationlist",
			bson.M{"$elemMatch": bson.M{
				"userlocation.nrlocation.tai.tac":    bson.D{{"$in", tacs}},
				"userlocation.nrlocation.tai.plmnid": bson.D{{"$in", plmnids}},
				"timestamp":                          timeStampCondition,
			}}},
		}
	}
	// combien the two filters
	filter := bson.D{{"$and", bson.A{filterTimeReg, filterLoc}}}
	return filter
}

// ------------------------------------------------------------------------------
// getFilterNwPerfNumPdu - Get filter that will be used to calculates the number PduSessionEst
func getFilterNwPerfNumPdu(engineReqData EngineReqData) bson.D {
	log.Printf("Constructing PDU session ratio filter for DB search ...")
	// get startTs and endTs
	startTs, endTs := getExtraReportReq(engineReqData)
	timeStampCondition := getTimeStampCondition(startTs, endTs)
	sessionFilter := bson.M{
		"timestamp": timeStampCondition,
	}
	// get Dnn filter
	if engineReqData.Dnns != nil {
		sessionFilter["dnn"] = bson.D{{"$in", engineReqData.Dnns}}
	}
	// get Snssai filter
	if engineReqData.Snssaia != nil {
		snssaiFilters := make(bson.A, 0, len(engineReqData.Snssaia))
		for _, snssai := range engineReqData.Snssaia {
			snssaiFilter := bson.M{
				"snssai.sst": snssai.Sst,
			}
			if snssai.Sd != "" {
				normalizedSd := strings.ToLower(snssai.Sd)
				snssaiFilter["snssai.sd"] = bson.D{{"$in", bson.A{
					normalizedSd,
					strings.ToUpper(normalizedSd),
				}}}
			}
			snssaiFilters = append(snssaiFilters, snssaiFilter)
		}
		if len(snssaiFilters) == 1 {
			for key, value := range snssaiFilters[0].(bson.M) {
				sessionFilter[key] = value
			}
		} else if len(snssaiFilters) > 1 {
			sessionFilter["$or"] = snssaiFilters
		}
	}
	return bson.D{{"pdusesestlist", bson.M{"$elemMatch": sessionFilter}}}
}
