/*
 * SPDX-License-Identifier: LicenseRef-CSSL-1.0
 */

/*
 * Functions of the analytics nbi service.
 */

package analytics

import (
	"bytes"
	"context"
	"encoding/json"
	"io/ioutil"
	"log"
	"net/http"
	"time"
)

// ------------------------------------------------------------------------------
type NWDAFAnalyticsDocumentApiService struct {
}

// ------------------------------------------------------------------------------
// Type of num_of_ue data to request engine
type EngineReqData struct {
	StartTs time.Time `json:"startTs,omitempty"`
	EndTs   time.Time `json:"endTs,omitempty"`
	Tais    []Tai     `json:"networkArea,omitempty"`
	Dnns    []string  `json:"dnns,omitempty"`
	Snssaia []Snssai  `json:"snssaia,omitempty"`
	Supi    string    `json:"supi,omitempty"`
	// NfInstanceIds forwards the TS 23.288 clause 6.5.1 Analytics Filter
	// Information "list of NF Instance IDs" to the engine, which owns the
	// identity of the NF it measures (resolved from the NRF per clause 6.2.2.4).
	NfInstanceIds []string `json:"nfInstanceIds,omitempty"`
	// Dnais, AppIds and UpfId forward the TS 23.288 Table 6.14.1-1 DN
	// Performance Analytics Filter Information ("DNAI", "Application ID",
	// "Anchor UPF info") to the engine.
	Dnais  []string `json:"dnais,omitempty"`
	AppIds []string `json:"appIds,omitempty"`
	UpfId  string   `json:"upfId,omitempty"`
	// OffsetPeriod forwards TS 29.520 EventReportingRequirement.offsetPeriod.
	// A positive value asks the engine for TS 23.288 Table 6.14.3-2
	// PREDICTIONS instead of Table 6.14.3-1 statistics.
	OffsetPeriod int32 `json:"offsetPeriod,omitempty"`
}

// ------------------------------------------------------------------------------
// Type of network_performance response from engine.
type NwPerfResp struct {
	RelativeRatio int32 `json:"relativeRatio,omitempty"`
	AbsoluteNum   int32 `json:"absoluteNum,omitempty"`
	Confidence    int32 `json:"confidence,omitempty"`
}

// ------------------------------------------------------------------------------
// Type of Ue_communication response from engine.
type UeCommResp struct {
	CommDur       int32     `json:"commDur"`
	Ts            time.Time `json:"ts,omitempty"`
	UlVol         int64     `json:"ulVol,omitempty"`
	UlVolVariance float32   `json:"ulVolVariance,omitempty"`
	DlVol         int64     `json:"dlVol,omitempty"`
	DlVolVariance float32   `json:"dlVolVariance,omitempty"`
}

// ------------------------------------------------------------------------------
// Type of Ue_mobility response from engine.
type UeMobResp struct {
	Loc []UserLocation `json:"loc"`
}

// ------------------------------------------------------------------------------
// Type of Nf_load response from engine.
type NfLoadResp struct {
	NfType       string `json:"nfType"`
	NfInstanceId string `json:"nfInstanceId"`
	// No omitempty: 0 is a valid load reading and must stay distinguishable
	// from "no reading". See PROJECT-HISTORY 14.5.
	NfCpuUsage         int32 `json:"nfCpuUsage"`
	NfMemoryUsage      int32 `json:"nfMemoryUsage"`
	NfLoadLevelAverage int32 `json:"nfLoadLevelAverage"`
	NfLoadLevelpeak    int32 `json:"nfLoadLevelpeak"`
	// PROJECT-INTERNAL diagnostics from the engine; deliberately NOT mapped
	// into the 3GPP NfLoadLevelInformation model. TS 23.288 Table 6.5.3-1
	// (statistics) defines no Confidence attribute.
	SampleCount    int32  `json:"sampleCount"`
	IdentitySource string `json:"identitySource,omitempty"`
}

// ------------------------------------------------------------------------------
// Type of Qos_sustainability response from engine.
type QosSustainabilityResp struct {
	RanUeThrouThd string `json:"ranUeThrouThd,omitempty"`
}

// ------------------------------------------------------------------------------
// Type of Dn_performance response from engine - TS 23.288 Table 6.14.3-1.
//
// SampleCount and the absence of any Confidence field are both deliberate:
// SampleCount is PROJECT-INTERNAL engine diagnostics and is NOT mapped into the
// 3GPP DnPerf model, and Table 6.14.3-1 (statistics) defines no Confidence
// attribute - only Table 6.14.3-2 (predictions) does, and this NWDAF does not
// produce predictions for DN_PERFORMANCE.
type DnPerfResp struct {
	AppId  string        `json:"appId,omitempty"`
	Dnn    string        `json:"dnn,omitempty"`
	Snssai *Snssai       `json:"snssai,omitempty"`
	DnPerf []DnPerfEntry `json:"dnPerf"`
	// Set by the engine ONLY for predictions (TS 23.288 Table 6.14.3-2).
	Confidence int32 `json:"confidence,omitempty"`
}

type DnPerfEntry struct {
	UpfId            string          `json:"upfId,omitempty"`
	Dnai             string          `json:"dnai,omitempty"`
	PerfData         PerfDataResp    `json:"perfData"`
	TemporalValidCon *TimeWindowResp `json:"temporalValidCon,omitempty"`
	SampleCount      int32           `json:"sampleCount,omitempty"`
	// PathHealth mirrors the engine's vendor-extension field. Kept as its own
	// type rather than reusing PathHealthExt so the engine wire format and the
	// NBI wire format stay independently changeable.
	PathHealth *PathHealthExt `json:"oaiPathHealthExt,omitempty"`
}

// PerfDataResp carries ONLY the two traffic-rate fields. The engine does not
// produce avePacketDelay / maxPacketDelay / avgPacketLossRate at all - see
// TS 23.288 Table 6.4.2-2 NOTE 1 - so there is nothing here to map them from,
// and they stay absent on the wire rather than being sent as zero.
type PerfDataResp struct {
	AvgTrafficRate string `json:"avgTrafficRate,omitempty"`
	MaxTrafficRate string `json:"maxTrafficRate,omitempty"`
}

type TimeWindowResp struct {
	StartTime time.Time `json:"startTime"`
	StopTime  time.Time `json:"stopTime"`
}

// ------------------------------------------------------------------------------
// Type of Traffic_steering response from oai-nwdaf-engine-traffic-steering.
type TrafficSteeringEngineResp struct {
	UpfId                 string  `json:"upfId"`
	CongestionScore       float32 `json:"congestionScore"`
	PredictedSlaViolation bool    `json:"predictedSlaViolation"`
	HorizonSec            int32   `json:"horizonSec,omitempty"`
	ModelVersion          string  `json:"modelVersion,omitempty"`
}

// ------------------------------------------------------------------------------
// NewNWDAFAnalyticsDocumentApiService - create a default api service
func NewNWDAFAnalyticsDocumentApiService() NWDAFAnalyticsDocumentApiServicer {
	return &NWDAFAnalyticsDocumentApiService{}
}

// ------------------------------------------------------------------------------
// GetNWDAFAnalytics - read a NWDAF Analytics
func (s *NWDAFAnalyticsDocumentApiService) GetNWDAFAnalytics(
	ctx context.Context,
	eventId EventIdAnyOf,
	anaReq EventReportingRequirement,
	eventFilter EventFilter,
	supportedFeatures string,
	tgtUe TargetUeInformation,
) (ImplResponse, error) {
	// Create AnalyticsData Report
	analyticsData := AnalyticsData{}
	// event-id is a REQUIRED query parameter (TS 29.520, GET /analytics).
	if eventId == "" {
		return ImplResponse{}, newProblem(
			http.StatusBadRequest, "MANDATORY_IE_MISSING",
			"the required query parameter event-id is absent")
	}
	// "empty" tracks whether the NWDAF has analytics to report. TS 29.520
	// GET /analytics defines "204 - No Content. The requested NWDAF Analytics
	// data does not exist" for exactly this case. Before, an empty result was
	// reported as 400 with a bare string, which tells a consumer its REQUEST
	// was malformed when in fact the request was fine and the DATA was absent.
	empty := false
	switch eventId {
	case EVENTIDANYOF_NETWORK_PERFORMANCE:
		nwPerfAnalyticsData, err := getNwPerfAnalytics(anaReq, eventFilter)
		if err != nil {
			return ImplResponse{}, err
		}
		empty = len(nwPerfAnalyticsData) == 0
		analyticsData.NwPerfs = nwPerfAnalyticsData

	case EVENTIDANYOF_UE_COMMUNICATION:
		ueCommsAnalyticsData, err := getUeCommsAnalytics(anaReq, eventFilter)
		if err != nil {
			return ImplResponse{}, err
		}
		empty = len(ueCommsAnalyticsData) == 0
		analyticsData.UeComms = ueCommsAnalyticsData

	case EVENTIDANYOF_UE_MOBILITY:
		ueMobAnalyticsData, err := getUeMobAnalytics(anaReq, eventFilter, tgtUe)
		if err != nil {
			return ImplResponse{}, err
		}
		empty = len(ueMobAnalyticsData) == 0
		analyticsData.UeMobs = ueMobAnalyticsData

	case EVENTIDANYOF_NF_LOAD:
		nfLoadAnalyticsData, err := getNfLoadAnalytics(anaReq, eventFilter)
		if err != nil {
			return ImplResponse{}, err
		}
		empty = len(nfLoadAnalyticsData) == 0
		analyticsData.NfLoadLevelInfos = nfLoadAnalyticsData

	case EVENTIDANYOF_QOS_SUSTAINABILITY:
		qosSustainAnalyticsData, err := getQosSustainabilityAnalytics(anaReq, eventFilter)
		if err != nil {
			return ImplResponse{}, err
		}
		empty = len(qosSustainAnalyticsData) == 0
		analyticsData.QosSustainInfos = qosSustainAnalyticsData

	case EVENTIDANYOF_DN_PERFORMANCE:
		dnPerfAnalyticsData, err := getDnPerformanceAnalytics(anaReq, eventFilter)
		if err != nil {
			return ImplResponse{}, err
		}
		empty = len(dnPerfAnalyticsData) == 0
		analyticsData.DnPerfInfos = dnPerfAnalyticsData

	case EVENTIDANYOF_TRAFFIC_STEERING_UPF_LOAD:
		// CUSTOM / NON-3GPP ANALYTICS EXTENSION - see model_event_id_any_of.go.
		trafficSteeringData, err := getTrafficSteeringAnalytics()
		if err != nil {
			return ImplResponse{}, err
		}
		empty = len(trafficSteeringData) == 0
		analyticsData.TrafficSteerings = trafficSteeringData

	default:
		// The Analytics ID is syntactically an EventId but this NWDAF does not
		// serve it. TS 23.288 clause 4.1 NOTE 1 permits an NWDAF to support a
		// subset; the NRF profile (nrf_registration.go) advertises exactly the
		// subset served, so a consumer that discovered this NWDAF properly will
		// not ask for anything else.
		return ImplResponse{}, newProblem(
			http.StatusBadRequest, "UNSUPPORTED_EVENT_ID",
			"Analytics ID "+string(eventId)+" is not supported by this NWDAF")
	}
	if empty {
		log.Printf("No analytics data for event-id=%s - returning 204", eventId)
		return Response(http.StatusNoContent, nil), nil
	}
	analyticsData.AnaMetaInfo.DataWindow.StartTime = anaReq.StartTs
	analyticsData.AnaMetaInfo.DataWindow.StopTime = anaReq.EndTs
	log.Printf("Returning NWDAF Analytics Data")
	return Response(http.StatusOK, analyticsData), nil
}

// ------------------------------------------------------------------------------
// getNwPerfAnalytics - Get list of NetworkPerfInfo
func getNwPerfAnalytics(
	anaReq EventReportingRequirement,
	eventFilter EventFilter,
) ([]NetworkPerfInfo, error) {
	log.Printf("Getting NW Performance Analytics")
	var nwPerfList []NetworkPerfInfo
	// for each NwPerfType, request the engine
	for _, nwPerfType := range eventFilter.NwPerfTypes {
		var nwPerfInfo NetworkPerfInfo
		var err error
		switch nwPerfType {
		case NETWORKPERFTYPEANYOF_NUM_OF_UE:
			nwPerfInfo, err = requestNwPerfEngine(
				anaReq,
				eventFilter,
				config.Engine.Uri+config.Routes.NumOfUe,
			)
			if err != nil {
				return nwPerfList, err
			}

		case NETWORKPERFTYPEANYOF_SESS_SUCC_RATIO:
			nwPerfInfo, err = requestNwPerfEngine(
				anaReq,
				eventFilter,
				config.Engine.Uri+config.Routes.SessSuccRatio,
			)
			if err != nil {
				return nwPerfList, err
			}

		default:
			return nil, newProblem(
				http.StatusBadRequest, "UNSUPPORTED_EVENT_FILTER",
				"NwPerfType "+string(nwPerfType)+" is not supported; this "+
					"NWDAF serves NUM_OF_UE and SESS_SUCC_RATIO only")
		}
		nwPerfInfo.NwPerfType = nwPerfType
		nwPerfList = append(nwPerfList, nwPerfInfo)
	}
	return nwPerfList, nil
}

// ------------------------------------------------------------------------------
// getUeCommNotifData - Get list Ue Communication
func getUeCommsAnalytics(
	anaReq EventReportingRequirement,
	eventFilter EventFilter,
) ([]UeCommunication, error) {

	log.Printf("Getting UE Communications Notification Data")
	var ueCommList []UeCommunication
	// this treat just one type of UE_COMMUNICATION
	var ueCommInfo UeCommunication
	var err error
	ueCommInfo, err = requestUeCommEngine(
		anaReq,
		eventFilter,
		config.Engine.Uri+config.Routes.UeComm,
	)
	if err != nil {
		return ueCommList, err
	}
	ueCommList = append(ueCommList, ueCommInfo)
	return ueCommList, nil
}

// ------------------------------------------------------------------------------
// getUeCommNotifData - Get list Ue Communication
func getUeMobAnalytics(
	anaReq EventReportingRequirement,
	eventFilter EventFilter,
	tgtUe TargetUeInformation,
) ([]UeMobility, error) {
	log.Printf("Getting UE Mobility Notification Data")
	var ueMobList []UeMobility
	// check supis not empty
	if len(tgtUe.Supis) == 0 {
		// TS 23.288 clause 6.7.1: UE mobility analytics require a Target of
		// Analytics Reporting (SUPI, Internal Group Id or "any UE"). Only the
		// SUPI form is implemented.
		return ueMobList, newProblem(
			http.StatusBadRequest, "MANDATORY_IE_MISSING",
			"UE_MOBILITY requires tgt-ue.supis (Target of Analytics Reporting)")
	}
	// for each User imsi, request the engine to get location.
	for _, supi := range tgtUe.Supis {
		var ueMobInfo UeMobility
		var err error
		ueMobInfo, err = requestUeMobEngine(
			anaReq,
			eventFilter,
			supi,
			config.Engine.Uri+config.Routes.UeMob,
		)
		if err != nil {
			return ueMobList, err
		}
		// TODO we need to add supi to the response ? ueMobInfo.Supi = supi
		ueMobList = append(ueMobList, ueMobInfo)
	}
	return ueMobList, nil
}

// ------------------------------------------------------------------------------
// getNfLoadAnalytics - NF load statistics, TS 23.288 clause 6.5.
//
// The clause 6.5.1 Analytics Filter Information "list of NF Instance IDs" is
// forwarded to the engine, which knows which NF instance it actually has data
// for (identity resolved from the NRF, clause 6.2.2.4). An EMPTY result is a
// legitimate answer and means the filter designated an NF this NWDAF cannot
// report on - it must NOT be turned into this NWDAF's own reading.
func getNfLoadAnalytics(
	anaReq EventReportingRequirement,
	eventFilter EventFilter,
) ([]NfLoadLevelInformation, error) {
	log.Printf("Getting NF Load Analytics")
	return requestNfLoadEngine(
		anaReq,
		eventFilter,
		config.Engine.Uri+config.Routes.NfLoad,
	)
}

// ------------------------------------------------------------------------------
// getQosSustainabilityAnalytics - Get list of QosSustainabilityInfo
func getQosSustainabilityAnalytics(
	anaReq EventReportingRequirement,
	eventFilter EventFilter,
) ([]QosSustainabilityInfo, error) {
	log.Printf("Getting Qos Sustainability Analytics")
	var qosSustainList []QosSustainabilityInfo
	qosSustainInfo, err := requestQosSustainabilityEngine(
		anaReq,
		eventFilter,
		config.Engine.Uri+config.Routes.QosSustainability,
	)
	if err != nil {
		return qosSustainList, err
	}
	qosSustainList = append(qosSustainList, qosSustainInfo)
	return qosSustainList, nil
}

// ------------------------------------------------------------------------------
// getDnPerformanceAnalytics - Get list of DnPerfInfo (TS 23.288 clause 6.14).
func getDnPerformanceAnalytics(
	anaReq EventReportingRequirement,
	eventFilter EventFilter,
) ([]DnPerfInfo, error) {
	log.Printf("Getting DN Performance Analytics")
	return requestDnPerformanceEngine(
		anaReq,
		eventFilter,
		config.Engine.Uri+config.Routes.DnPerformance,
	)
}

// ------------------------------------------------------------------------------
// requestDnPerformanceEngine - ask the engine and map its answer onto the 3GPP
// DnPerfInfo model of TS 23.288 Table 6.14.3-1.
//
// The whole Table 6.14.1-1 filter set that this NWDAF can act on is forwarded:
// DNAI, Anchor UPF info, Application ID, DNN and S-NSSAI. Two filters are NOT
// forwarded and the reason is not laziness:
//   - Area of Interest: no per-area scoping exists in this deployment, so a
//     spatial filter could not be honoured.
//   - appServerAddrs / dnPerfReqs: appServerAddrs needs per-application-server
//     classification, and dnPerfReqs (dnPerfOrderCriter / order /
//     reportThresholds) is a CONSUMER-side ordering preference whose evaluation
//     is explicitly deferred. Silently ignoring a filter the consumer set would
//     let it believe a constraint was applied when it was not.
func requestDnPerformanceEngine(
	anaReq EventReportingRequirement,
	eventFilter EventFilter,
	enginePath string,
) ([]DnPerfInfo, error) {
	log.Printf("Reaching engine to get DN Performance Info from DB")
	var engineReqData EngineReqData
	engineReqData.StartTs = anaReq.StartTs
	engineReqData.EndTs = anaReq.EndTs
	engineReqData.Dnais = eventFilter.Dnais
	engineReqData.Dnns = eventFilter.Dnns
	engineReqData.Snssaia = eventFilter.Snssais
	engineReqData.AppIds = eventFilter.AppIds
	engineReqData.UpfId = eventFilter.UpfId
	// TS 29.520: "a positive value means prediction in the future offset
	// period". Forwarded verbatim; the engine decides what it can honour.
	engineReqData.OffsetPeriod = anaReq.OffsetPeriod
	engineReqJsonData, err := json.Marshal(engineReqData)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(
		http.MethodGet,
		enginePath,
		bytes.NewBuffer(engineReqJsonData),
	)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := engineClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var engineList []DnPerfResp
	if err := json.Unmarshal(body, &engineList); err != nil {
		return nil, err
	}
	dnPerfInfos := make([]DnPerfInfo, 0, len(engineList))
	for _, r := range engineList {
		perfs := make([]DnPerf, 0, len(r.DnPerf))
		for _, p := range r.DnPerf {
			log.Printf(
				"DN_PERFORMANCE: dnn=%s dnai=%s upfId=%s avgTrafficRate=%s "+
					"maxTrafficRate=%s samples=%d",
				r.Dnn, p.Dnai, p.UpfId, p.PerfData.AvgTrafficRate,
				p.PerfData.MaxTrafficRate, p.SampleCount)
			dnPerf := DnPerf{
				UpfId: p.UpfId,
				Dnai:  p.Dnai,
				PerfData: PerfData{
					AvgTrafficRate: p.PerfData.AvgTrafficRate,
					MaxTrafficRate: p.PerfData.MaxTrafficRate,
					// AvePacketDelay / MaxPacketDelay / AvgPacketLossRate are
					// deliberately left at their zero value so that omitempty
					// keeps them OFF THE WIRE. They are not measurable here
					// (TS 23.288 Table 6.4.2-2 NOTE 1) and a zero would be read
					// as "measured zero delay / zero loss".
				},
			}
			// Vendor extension, forwarded verbatim. Deliberately NOT folded
			// into PerfData - see model_path_health_ext.go.
			if p.PathHealth != nil {
				dnPerf.PathHealthExt = &PathHealthExt{
					State:                  p.PathHealth.State,
					SendtoFailurePerPacket: p.PathHealth.SendtoFailurePerPacket,
					TxAttempts:             p.PathHealth.TxAttempts,
					SendtoFailures:         p.PathHealth.SendtoFailures,
					Denominator:            p.PathHealth.Denominator,
					Semantics:              p.PathHealth.Semantics,
					ObservedAt:             p.PathHealth.ObservedAt,
					AgeSec:                 p.PathHealth.AgeSec,
				}
			}
			if p.TemporalValidCon != nil {
				dnPerf.TemporalValidCon = TimeWindow{
					StartTime: p.TemporalValidCon.StartTime,
					StopTime:  p.TemporalValidCon.StopTime,
				}
			}
			perfs = append(perfs, dnPerf)
		}
		if len(perfs) == 0 {
			continue
		}
		info := DnPerfInfo{
			Dnn:    r.Dnn,
			DnPerf: perfs,
			// AppId is NOT set: the engine never classifies traffic per
			// application, and echoing back the requested appId would assert an
			// application scope the measurement does not have.
			//
			// Confidence is set ONLY when the engine produced a PREDICTION
			// (TS 23.288 Table 6.14.3-2). For statistics the engine leaves it
			// zero and it stays off the wire, because Table 6.14.3-1 defines no
			// such attribute.
			Confidence: r.Confidence,
		}
		if r.Snssai != nil {
			info.Snssai = Snssai{Sst: r.Snssai.Sst, Sd: r.Snssai.Sd}
		}
		dnPerfInfos = append(dnPerfInfos, info)
	}
	return dnPerfInfos, nil
}

// ------------------------------------------------------------------------------
// getTrafficSteeringAnalytics - Get list of TrafficSteeringInfo (currently just the
// engine's configured UPF).
func getTrafficSteeringAnalytics() ([]TrafficSteeringInfo, error) {
	log.Printf("Getting Traffic Steering Analytics")
	var trafficSteeringList []TrafficSteeringInfo
	trafficSteeringInfo, err := requestTrafficSteeringEngine(
		config.Engine.TrafficSteeringUri + config.Routes.TrafficSteering,
	)
	if err != nil {
		return trafficSteeringList, err
	}
	trafficSteeringList = append(trafficSteeringList, trafficSteeringInfo)
	return trafficSteeringList, nil
}

// ------------------------------------------------------------------------------
func requestTrafficSteeringEngine(enginePath string) (TrafficSteeringInfo, error) {
	log.Printf("Reaching engine to get Traffic Steering congestion forecast")
	req, err := http.NewRequest(http.MethodGet, enginePath, nil)
	if err != nil {
		return TrafficSteeringInfo{}, err
	}
	resp, err := engineClient().Do(req)
	if err != nil {
		return TrafficSteeringInfo{}, err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	log.Println(string(body))
	var tsResp TrafficSteeringEngineResp
	err = json.Unmarshal(body, &tsResp)
	if err != nil {
		return TrafficSteeringInfo{}, err
	}
	return TrafficSteeringInfo{
		UpfId:                 tsResp.UpfId,
		CongestionScore:       tsResp.CongestionScore,
		PredictedSlaViolation: tsResp.PredictedSlaViolation,
		HorizonSec:            tsResp.HorizonSec,
		ModelVersion:          tsResp.ModelVersion,
	}, nil
}

// ------------------------------------------------------------------------------
func requestNfLoadEngine(
	anaReq EventReportingRequirement,
	eventFilter EventFilter,
	enginePath string,
) ([]NfLoadLevelInformation, error) {
	log.Printf("Reaching engine to get NF Load Info from DB")
	var engineReqData EngineReqData
	engineReqData.StartTs = anaReq.StartTs
	engineReqData.EndTs = anaReq.EndTs
	engineReqData.NfInstanceIds = eventFilter.NfInstanceIds
	engineReqJsonData, err := json.Marshal(engineReqData)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(
		http.MethodGet,
		enginePath,
		bytes.NewBuffer(engineReqJsonData),
	)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := engineClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var engineList []NfLoadResp
	if err := json.Unmarshal(body, &engineList); err != nil {
		return nil, err
	}
	nfLoadList := make([]NfLoadLevelInformation, 0, len(engineList))
	for _, r := range engineList {
		log.Printf(
			"NF_LOAD: nfInstanceId=%s (identity source: %s) avg=%d peak=%d "+
				"cpu=%d mem=%d samples=%d",
			r.NfInstanceId, r.IdentitySource, r.NfLoadLevelAverage,
			r.NfLoadLevelpeak, r.NfCpuUsage, r.NfMemoryUsage, r.SampleCount)
		nfLoadList = append(nfLoadList, NfLoadLevelInformation{
			NfType:             NfType(r.NfType),
			NfInstanceId:       r.NfInstanceId,
			NfCpuUsage:         r.NfCpuUsage,
			NfMemoryUsage:      r.NfMemoryUsage,
			NfLoadLevelAverage: r.NfLoadLevelAverage,
			NfLoadLevelpeak:    r.NfLoadLevelpeak,
			// Confidence is deliberately NOT set: TS 23.288 Table 6.5.3-1
			// (NF load statistics) defines no such attribute. It exists only
			// in Table 6.5.3-2 (predictions), which this NWDAF does not
			// produce for NF_LOAD.
		})
	}
	return nfLoadList, nil
}

// ------------------------------------------------------------------------------
func requestQosSustainabilityEngine(
	anaReq EventReportingRequirement,
	eventFilter EventFilter,
	enginePath string,
) (QosSustainabilityInfo, error) {
	log.Printf("Reaching engine to get Qos Sustainability Info from DB")
	var engineReqData EngineReqData
	engineReqData.StartTs = anaReq.StartTs
	engineReqData.EndTs = anaReq.EndTs
	engineReqData.Snssaia = eventFilter.Snssais
	engineReqJsonData, err := json.Marshal(engineReqData)
	if err != nil {
		return QosSustainabilityInfo{}, err
	}
	req, err := http.NewRequest(
		http.MethodGet,
		enginePath,
		bytes.NewBuffer(engineReqJsonData),
	)
	if err != nil {
		return QosSustainabilityInfo{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := engineClient().Do(req)
	if err != nil {
		return QosSustainabilityInfo{}, err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	log.Println(string(body))
	var qosSustainResp QosSustainabilityResp
	err = json.Unmarshal(body, &qosSustainResp)
	if err != nil {
		return QosSustainabilityInfo{}, err
	}
	qosSustainInfo := QosSustainabilityInfo{
		StartTs:       anaReq.StartTs,
		EndTs:         anaReq.EndTs,
		RanUeThrouThd: qosSustainResp.RanUeThrouThd,
		// Confidence deliberately unset - TS 23.288 Table 6.9.3-1
		// ("QoS Sustainability" statistics) defines no Confidence attribute.
	}
	// Only set AreaInfo when the consumer actually supplied one. Taking the
	// address unconditionally made a zero-value NetworkAreaInfo non-nil, so
	// "areaInfo":{} was emitted on every response - the same present-but-empty
	// problem the pointer change was meant to remove.
	if len(eventFilter.NetworkArea.Tais) > 0 || len(eventFilter.NetworkArea.Ecgis) > 0 ||
		len(eventFilter.NetworkArea.Ncgis) > 0 || len(eventFilter.NetworkArea.GRanNodeIds) > 0 {
		area := eventFilter.NetworkArea
		qosSustainInfo.AreaInfo = &area
	}
	if len(eventFilter.Snssais) > 0 {
		qosSustainInfo.Snssai = &eventFilter.Snssais[0]
	}
	return qosSustainInfo, nil
}

// ------------------------------------------------------------------------------
func requestNwPerfEngine(
	anaReq EventReportingRequirement,
	eventFilter EventFilter,
	enginePath string,
) (NetworkPerfInfo, error) {
	log.Printf("Reaching engine to get Network Performance Info from DB")
	var engineReqData EngineReqData
	engineReqData.StartTs = anaReq.StartTs
	engineReqData.EndTs = anaReq.EndTs
	// for num_of_ue
	engineReqData.Tais = eventFilter.NetworkArea.Tais
	// for sess_succ_ratio request
	engineReqData.Dnns = eventFilter.Dnns
	engineReqData.Snssaia = eventFilter.Snssais
	// Convert the data to a JSON byte array
	engineReqJsonData, err := json.Marshal(engineReqData)
	if err != nil {
		return NetworkPerfInfo{}, err
	}
	// Create a POST request with the JSON data in the body
	req, err := http.NewRequest(
		http.MethodGet,
		enginePath, bytes.NewBuffer(engineReqJsonData))
	if err != nil {
		return NetworkPerfInfo{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	// Send the request and print the response body
	resp, err := engineClient().Do(req)
	if err != nil {
		return NetworkPerfInfo{}, err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	log.Println(string(body))
	var nwPerfResp NwPerfResp
	err = json.Unmarshal(body, &nwPerfResp)
	if err != nil {
		return NetworkPerfInfo{}, err
	}
	nwPerfInfo := NetworkPerfInfo{
		NetworkArea:   eventFilter.NetworkArea,
		AbsoluteNum:   &nwPerfResp.AbsoluteNum,
		RelativeRatio: &nwPerfResp.RelativeRatio,
		Confidence:    &nwPerfResp.Confidence,
	}
	return nwPerfInfo, nil
}

// ------------------------------------------------------------------------------
func requestUeCommEngine(
	anaReq EventReportingRequirement,
	eventFilter EventFilter,
	enginePath string,
) (UeCommunication, error) {
	log.Printf("Reaching engine to get UE Communication Info from DB")
	var engineReqData EngineReqData
	engineReqData.StartTs = anaReq.StartTs
	engineReqData.EndTs = anaReq.EndTs
	// Convert the data to a JSON byte array
	engineReqJsonData, err := json.Marshal(engineReqData)
	if err != nil {
		return UeCommunication{}, err
	}
	// Create a POST request with the JSON data in the body
	req, err := http.NewRequest(
		http.MethodGet,
		enginePath,
		bytes.NewBuffer(engineReqJsonData),
	)
	if err != nil {
		return UeCommunication{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	// Send the request and print the response body
	resp, err := engineClient().Do(req)
	if err != nil {
		return UeCommunication{}, err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	log.Println(string(body))
	var ueCommResp UeCommResp
	err = json.Unmarshal(body, &ueCommResp)
	if err != nil {
		return UeCommunication{}, err
	}
	trafChar := TrafficCharacterization{
		UlVol:         &ueCommResp.UlVol,
		UlVolVariance: &ueCommResp.UlVolVariance,
		DlVol:         &ueCommResp.DlVol,
		DlVolVariance: &ueCommResp.DlVolVariance,
	}
	ueCommunication := UeCommunication{
		CommDur:  ueCommResp.CommDur,
		Ts:       anaReq.StartTs,
		TrafChar: trafChar,
	}
	return ueCommunication, nil
}

// ------------------------------------------------------------------------------
func requestUeMobEngine(
	anaReq EventReportingRequirement,
	eventFilter EventFilter,
	supi string,
	enginePath string,
) (UeMobility, error) {
	log.Printf("Reaching engine to get UE Mobility Info from DB")
	log.Printf("Supi : %s", supi)
	var engineReqData EngineReqData
	engineReqData.StartTs = anaReq.StartTs
	engineReqData.EndTs = anaReq.EndTs
	engineReqData.Supi = supi
	// Convert the data to a JSON byte array
	engineReqJsonData, err := json.Marshal(engineReqData)
	if err != nil {
		return UeMobility{}, err
	}
	// Create a POST request with the JSON data in the body
	req, err := http.NewRequest(
		http.MethodGet,
		enginePath,
		bytes.NewBuffer(engineReqJsonData),
	)
	if err != nil {
		return UeMobility{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	// Send the request and print the response body
	resp, err := engineClient().Do(req)
	if err != nil {
		return UeMobility{}, err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	var ueMobResp UeMobResp
	err = json.Unmarshal(body, &ueMobResp)
	if err != nil {
		return UeMobility{}, err
	}
	// Create a variable of type UeMobility
	var ueMobility UeMobility
	// Fill the Ts field with the current time, and duration
	ueMobility.Ts = time.Now()
	ueMobility.Duration = 10
	// Iterate over the Loc slice in UeMobResp
	for _, userLocation := range ueMobResp.Loc {
		locationInfo := LocationInfo{
			Loc:        userLocation,
			Ratio:      100, // Set the ratio to 100 as an example, you can change it as needed
			Confidence: 0,   // Set the confidence to 0 as an example, you can change it as needed
		}
		ueMobility.LocInfos = append(ueMobility.LocInfos, locationInfo)
	}
	return ueMobility, nil
}
