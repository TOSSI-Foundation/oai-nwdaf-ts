/*
 * SPDX-License-Identifier: LicenseRef-CSSL-1.0
 */

/*
 * Functions of the events nbi service (create subscription).
 */

package events

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"
)

/*
NWDAFEventsSubscriptionsCollectionApiService is a service that implements
the logic for the NWDAFEventsSubscriptionsCollectionApiServicer
*/
type NWDAFEventsSubscriptionsCollectionApiService struct {
}

// ------------------------------------------------------------------------------
// Type of num_of_ue data to request engine
type EngineReqData struct {
	StartTs time.Time `json:"startTs,omitempty"`
	EndTs   time.Time `json:"endTs,omitempty"`
	// for num_of_ue
	Tais []Tai `json:"networkArea,omitempty"`
	// for sess_succ_ratio
	Dnns    []string `json:"dnns,omitempty"`
	Snssaia []Snssai `json:"snssaia,omitempty"`
	Supi    string   `json:"supi,omitempty"`
	// NfInstanceIds forwards the TS 23.288 clause 6.5.1 Analytics Filter
	// Information "list of NF Instance IDs" to the engine.
	NfInstanceIds []string `json:"nfInstanceIds,omitempty"`
	// Dnais, AppIds and UpfId forward the TS 23.288 Table 6.14.1-1 DN
	// Performance Analytics Filter Information ("DNAI", "Application ID",
	// "Anchor UPF info") to the engine.
	Dnais  []string `json:"dnais,omitempty"`
	AppIds []string `json:"appIds,omitempty"`
	UpfId  string   `json:"upfId,omitempty"`
	// TS 29.520 EventReportingRequirement.offsetPeriod - positive asks for
	// TS 23.288 Table 6.14.3-2 predictions.
	OffsetPeriod int32 `json:"offsetPeriod,omitempty"`
}

// ------------------------------------------------------------------------------
// Types of Dn_performance response from engine - TS 23.288 Table 6.14.3-1.
// SampleCount is PROJECT-INTERNAL engine diagnostics and is NOT mapped into the
// 3GPP DnPerf model. There is no Confidence field: Table 6.14.3-1 (statistics)
// defines none, only Table 6.14.3-2 (predictions) does.
type DnPerfResp struct {
	AppId  string        `json:"appId,omitempty"`
	Dnn    string        `json:"dnn,omitempty"`
	Snssai *Snssai       `json:"snssai,omitempty"`
	DnPerf []DnPerfEntry `json:"dnPerf"`
	// Set ONLY for predictions (TS 23.288 Table 6.14.3-2).
	Confidence int32 `json:"confidence,omitempty"`
}

type DnPerfEntry struct {
	UpfId            string          `json:"upfId,omitempty"`
	Dnai             string          `json:"dnai,omitempty"`
	PerfData         PerfDataResp    `json:"perfData"`
	TemporalValidCon *TimeWindowResp `json:"temporalValidCon,omitempty"`
	SampleCount      int32           `json:"sampleCount,omitempty"`
}

// PerfDataResp carries ONLY the two traffic-rate fields - avePacketDelay,
// maxPacketDelay and avgPacketLossRate are not measurable in this deployment
// (TS 23.288 Table 6.4.2-2 NOTE 1) and stay absent rather than being sent as 0.
type PerfDataResp struct {
	AvgTrafficRate string `json:"avgTrafficRate,omitempty"`
	MaxTrafficRate string `json:"maxTrafficRate,omitempty"`
}

type TimeWindowResp struct {
	StartTime time.Time `json:"startTime"`
	StopTime  time.Time `json:"stopTime"`
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
// Type of Ue_communication response from engine.
type AbnorBehavrsResp struct {
	Ratio int32 `json:"ratio,omitempty"`
}

// ------------------------------------------------------------------------------
// Type of Nf_load response from engine.
type NfLoadResp struct {
	NfType       string `json:"nfType"`
	NfInstanceId string `json:"nfInstanceId"`
	// No omitempty: 0 is a valid load reading (PROJECT-HISTORY 14.5).
	NfCpuUsage         int32 `json:"nfCpuUsage"`
	NfMemoryUsage      int32 `json:"nfMemoryUsage"`
	NfLoadLevelAverage int32 `json:"nfLoadLevelAverage"`
	NfLoadLevelpeak    int32 `json:"nfLoadLevelpeak"`
	// PROJECT-INTERNAL diagnostics; NOT mapped into NfLoadLevelInformation.
	// TS 23.288 Table 6.5.3-1 (statistics) defines no Confidence attribute.
	SampleCount    int32  `json:"sampleCount"`
	IdentitySource string `json:"identitySource,omitempty"`
}

// ------------------------------------------------------------------------------
// Type of Qos_sustainability response from engine.
type QosSustainabilityResp struct {
	RanUeThrouThd string `json:"ranUeThrouThd,omitempty"`
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

// NewNWDAFEventsSubscriptionsCollectionApiService creates a default api service
func NewNWDAFEventsSubscriptionsCollectionApiService() NWDAFEventsSubscriptionsCollectionApiServicer {
	return &NWDAFEventsSubscriptionsCollectionApiService{}
}

// ------------------------------------------------------------------------------
// supportedEvents - the Analytics IDs this NWDAF actually dispatches.
//
// This is the same list nrf_registration.go advertises in the NRF NF profile,
// minus nothing: what is advertised and what is served must be identical, or
// discovery becomes actively misleading.
//
// TRAFFIC_STEERING_UPF_LOAD is served but is a CUSTOM / NON-3GPP ANALYTICS
// EXTENSION (see model_nwdaf_event.go). It is deliberately NOT advertised in
// the NRF profile.
func eventIsSupported(e NwdafEvent) bool {
	switch e {
	case NWDAFEVENT_NETWORK_PERFORMANCE,
		NWDAFEVENT_UE_COMMUNICATION,
		NWDAFEVENT_UE_MOBILITY,
		NWDAFEVENT_NF_LOAD,
		NWDAFEVENT_QOS_SUSTAINABILITY,
		NWDAFEVENT_ABNORMAL_BEHAVIOUR,
		NWDAFEVENT_DN_PERFORMANCE,
		NWDAFEVENT_TRAFFIC_STEERING_UPF_LOAD:
		return true
	}
	return false
}

// ------------------------------------------------------------------------------
// checkEventFeasibility - TS 23.288 clause 6.1.1.1 step 1, "the NWDAF
// determines whether triggering new data collection is needed", and TS 29.520's
// failEventReports mechanism: an event the NWDAF cannot serve is reported back
// in FailureEventInfo{event, failureCode} rather than accepted and dropped.
//
// Returns an empty string when the event is accepted, otherwise the
// NwdafFailureCode to report.
func checkEventFeasibility(eventSub EventSubscription) NwdafFailureCode {
	if !eventIsSupported(eventSub.Event) {
		// The Analytics ID is not one this NWDAF serves. TS 23.288 clause 4.1
		// NOTE 1 explicitly allows an NWDAF to support a subset.
		return NWDAFFAILURECODE_UNAVAILABLE_DATA
	}
	// THRESHOLD is the second of the two values NotificationMethod defines.
	// It requires evaluating the Reporting Thresholds of clause 6.1.3
	// (nfLoadLvlThds / loadLevelThreshold, matchingDir, and the acceptable
	// deviation) against each computed analytic. That is NOT implemented.
	//
	// Previously a THRESHOLD subscription was answered 201 Created and then
	// went permanently silent, because the delivery goroutine hit its default
	// branch, logged "Not implemented yet" and broke out of the loop. A
	// consumer had no way to learn that. Reporting it in failEventReports is
	// the specification's own mechanism for saying so.
	// CLASSIFICATION: Known non-compliance, now declared rather than silent.
	if eventSub.NotificationMethod == NOTIFICATIONMETHOD_THRESHOLD {
		return NWDAFFAILURECODE_OTHER
	}
	return ""
}

// CreateNWDAFEventsSubscription - Create a new Individual NWDAF Events Subscription
// (TS 23.288 clause 6.1.1.1, TS 29.520 POST /subscriptions).
func (s *NWDAFEventsSubscriptionsCollectionApiService) CreateNWDAFEventsSubscription(
	ctx context.Context,
	nnwdafEventsSubscription NnwdafEventsSubscription,
	urlBasePath string,
) (ImplResponse, error) {
	if len(nnwdafEventsSubscription.EventSubscriptions) == 0 {
		// eventSubscriptions is `required` with minItems: 1 in TS 29.520.
		return ImplResponse{}, newProblem(
			http.StatusBadRequest, "MANDATORY_IE_MISSING",
			"eventSubscriptions is required and must contain at least one entry")
	}
	if nnwdafEventsSubscription.NotificationURI == "" {
		return ImplResponse{}, newProblem(
			http.StatusBadRequest, "MANDATORY_IE_MISSING",
			"notificationURI (Notification Target Address, TS 23.288 clause "+
				"6.1.3) is required for a subscribe/notify subscription")
	}

	// Feasibility check BEFORE creating the resource.
	accepted := make([]EventSubscription, 0, len(nnwdafEventsSubscription.EventSubscriptions))
	failed := make([]FailureEventInfo, 0)
	for _, eventSub := range nnwdafEventsSubscription.EventSubscriptions {
		if code := checkEventFeasibility(eventSub); code != "" {
			log.Printf(
				"Subscription rejected event %s: failureCode=%s",
				eventSub.Event, code)
			failed = append(failed, FailureEventInfo{
				Event:       eventSub.Event,
				FailureCode: code,
			})
			continue
		}
		accepted = append(accepted, eventSub)
	}
	if len(accepted) == 0 {
		// Every requested event was refused, so there is nothing to subscribe
		// to and no resource is created. TS 29.520 does not state what to do
		// when the whole subscription is infeasible; creating a resource that
		// will never notify would be worse than an explicit rejection.
		// CLASSIFICATION: Project-specific decision where the specification is
		// silent - the per-event reason is still returned in ProblemDetails.
		detail := "no requested event can be served by this NWDAF:"
		for _, f := range failed {
			detail += " " + string(f.Event) + "=" + string(f.FailureCode)
		}
		return ImplResponse{}, newProblem(
			http.StatusBadRequest, "UNSUPPORTED_EVENT_ID", detail)
	}

	sub := &subscription{
		id:              uuid.New().String(),
		notifCorrId:     nnwdafEventsSubscription.NotifCorrId,
		notificationURI: nnwdafEventsSubscription.NotificationURI,
		cancel:          make(chan string),
	}
	subscriptions.add(sub)
	// NOT ctx. An Individual NWDAF Events Subscription outlives the HTTP
	// request that created it, but ctx is the SUBSCRIBE request's context and
	// is cancelled the moment that request completes - every subsequent
	// notification then failed instantly with "context canceled". The
	// subscription's own cancel channel is what governs the goroutine's
	// lifetime; see subscriptions.remove().
	for _, eventSub := range accepted {
		go handleSubscriptionEvent(context.Background(), eventSub, sub)
	}
	log.Printf(
		"Created subscription %s (notifCorrId=%q, %d event(s) accepted, "+
			"%d rejected, %d live subscriptions)",
		sub.id, sub.notifCorrId, len(accepted), len(failed), subscriptions.count())

	// TS 29.520 POST /subscriptions 201: the Location header is `required` and
	// carries {apiRoot}/nnwdaf-eventssubscription/<apiVersion>/subscriptions/{subscriptionId}.
	respHeaders := make(map[string][]string)
	respHeaders["Location"] = []string{
		config.Events.Uri + urlBasePath + "/" + sub.id,
	}
	eventSubInfo := NnwdafEventsSubscription{
		EventSubscriptions: accepted,
		NotifCorrId:        nnwdafEventsSubscription.NotifCorrId,
		NotificationURI:    nnwdafEventsSubscription.NotificationURI,
	}
	if len(failed) > 0 {
		eventSubInfo.FailEventReports = failed
	}
	return ResponseWithHeaders(201, respHeaders, eventSubInfo), nil
}

// ------------------------------------------------------------------------------
// minRepetitionPeriodSec - floor for the consumer-supplied repetitionPeriod.
// A subscription that omits it (or sends 0) previously produced a delivery loop
// with time.Sleep(0), i.e. an unthrottled hot loop hammering both the engine
// and the consumer's callback. TS 29.520 types repetitionPeriod as a
// DurationSec and gives no lower bound, so this floor is a PROJECT-SPECIFIC
// safety limit, not a specification requirement.
const minRepetitionPeriodSec = 1

// ------------------------------------------------------------------------------
// handleSubscriptionEvent - deliver one subscribed event periodically until the
// subscription is cancelled. Only feasible events reach here; infeasibility is
// reported at subscribe time through failEventReports (checkEventFeasibility).
func handleSubscriptionEvent(
	ctx context.Context,
	eventSub EventSubscription,
	sub *subscription,
) {
	period := time.Duration(eventSub.RepetitionPeriod) * time.Second
	if eventSub.RepetitionPeriod < minRepetitionPeriodSec {
		period = minRepetitionPeriodSec * time.Second
		log.Printf(
			"Subscription %s event %s: repetitionPeriod %d is below the %ds "+
				"floor, using %ds",
			sub.id, eventSub.Event, eventSub.RepetitionPeriod,
			minRepetitionPeriodSec, minRepetitionPeriodSec)
	}
	log.Print("Handling subscription to ", eventSub.Event,
		" with subscription id ", sub.id)
loop:
	for {
		select {
		case <-sub.cancel:
			break loop
		default:
		}

		eventNotif, err := fillEventNotification(ctx, eventSub)
		if err != nil {
			// A transient engine failure must not silently end the
			// subscription: the consumer would never be told. Log and retry on
			// the next period.
			log.Printf(
				"Subscription %s event %s: could not build notification: %v "+
					"(retrying in %s)", sub.id, eventSub.Event, err, period)
		} else if err = sendNotification(ctx, eventNotif, sub); err != nil {
			log.Printf(
				"Subscription %s event %s: delivery to %s failed: %v "+
					"(retrying in %s)",
				sub.id, eventSub.Event, sub.notificationURI, err, period)
		}

		select {
		case <-sub.cancel:
			break loop
		case <-time.After(period):
		}
	}
	log.Print("subscription to ", eventSub.Event,
		" with subscription id ", sub.id, " is closed.")
}

// ------------------------------------------------------------------------------
// fillEventNotification - return event notification information
func fillEventNotification(ctx context.Context,
	eventSub EventSubscription,
) (EventNotification, error) {
	// only NETWORK_PERFORMACE - NUM_OF_UE is implemented for the moment
	var eventNotif EventNotification
	switch eventSub.Event {

	case NWDAFEVENT_NETWORK_PERFORMANCE:

		nwPerfNotifData, err := getNwPerfNotifData(eventSub)
		if err != nil {
			return eventNotif, err
		}
		eventNotif.NwPerfs = nwPerfNotifData

	case NWDAFEVENT_UE_COMMUNICATION:

		UeCommData, err := getUeCommNotifData(eventSub)
		if err != nil {
			return eventNotif, err
		}
		eventNotif.UeComms = UeCommData

	case NWDAFEVENT_UE_MOBILITY:

		UeMobData, err := getUeMobNotifData(eventSub)
		if err != nil {
			return eventNotif, err
		}
		eventNotif.UeMobs = UeMobData

	case NWDAFEVENT_ABNORMAL_BEHAVIOUR:

		AbnorBehavrsData, err := getAbnormalBehaviourNotifData(eventSub)
		if err != nil {
			return eventNotif, err
		}
		eventNotif.AbnorBehavrs = AbnorBehavrsData

	case NWDAFEVENT_NF_LOAD:

		NfLoadData, err := getNfLoadNotifData(eventSub)
		if err != nil {
			return eventNotif, err
		}
		eventNotif.NfLoadLevelInfos = NfLoadData

	case NWDAFEVENT_QOS_SUSTAINABILITY:

		QosSustainData, err := getQosSustainabilityNotifData(eventSub)
		if err != nil {
			return eventNotif, err
		}
		eventNotif.QosSustainInfos = QosSustainData

	case NWDAFEVENT_DN_PERFORMANCE:

		DnPerfData, err := getDnPerformanceNotifData(eventSub)
		if err != nil {
			return eventNotif, err
		}
		eventNotif.DnPerfInfos = DnPerfData

	case NWDAFEVENT_TRAFFIC_STEERING_UPF_LOAD:

		TrafficSteeringData, err := getTrafficSteeringNotifData()
		if err != nil {
			return eventNotif, err
		}
		eventNotif.TrafficSteerings = TrafficSteeringData

	default:
		// Implement others
		log.Print("Not implemented yet")
	}
	eventNotif.Event = eventSub.Event
	eventNotif.AnaMetaInfo.DataWindow.StartTime = eventSub.ExtraReportReq.StartTs
	eventNotif.AnaMetaInfo.DataWindow.StopTime = eventSub.ExtraReportReq.EndTs
	return eventNotif, nil
}

// ------------------------------------------------------------------------------
// getNwPerfAnalytics - Get list of NetworkPerfInfo
func getNwPerfNotifData(eventSub EventSubscription) ([]NetworkPerfInfo, error) {
	log.Printf("Getting NW Performance Notification Data")
	var nwPerfList []NetworkPerfInfo
	for _, nwPerfReq := range eventSub.NwPerfRequs {
		var nwPerfInfo NetworkPerfInfo
		var err error
		switch nwPerfReq.NwPerfType {

		case NETWORKPERFTYPE_NUM_OF_UE:
			nwPerfInfo, err = requestNwPerfEngine(
				eventSub,
				config.Engine.Uri+config.Routes.NumOfUe,
			)
			if err != nil {
				return nwPerfList, err
			}

		case NETWORKPERFTYPE_SESS_SUCC_RATIO:
			nwPerfInfo, err = requestNwPerfEngine(
				eventSub,
				config.Engine.Uri+config.Routes.SessSuccRatio,
			)
			if err != nil {
				return nwPerfList, err
			}

		default:
			return nil, newProblem(
				http.StatusBadRequest, "UNSUPPORTED_EVENT_FILTER",
				"NwPerfType "+string(nwPerfReq.NwPerfType)+" is not supported; "+
					"this NWDAF serves NUM_OF_UE and SESS_SUCC_RATIO only")
		}
		nwPerfInfo.NwPerfType = nwPerfReq.NwPerfType
		nwPerfList = append(nwPerfList, nwPerfInfo)
	}
	return nwPerfList, nil
}

// ------------------------------------------------------------------------------
// getUeCommNotifData - Get list Ue Communication
func getUeCommNotifData(eventSub EventSubscription) ([]UeCommunication, error) {
	log.Printf("Getting UE Communications Notification Data")
	var ueCommList []UeCommunication
	// this treat just one type of UE_COMMUNICATION
	var ueCommInfo UeCommunication
	var err error
	ueCommInfo, err = requestUeCommEngine(
		eventSub,
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
func getUeMobNotifData(eventSub EventSubscription) ([]UeMobility, error) {
	log.Printf("Getting UE Mobility Notification Data")
	var ueMobList []UeMobility
	// check supis not empty
	if len(eventSub.TgtUe.Supis) == 0 {
		return ueMobList, newProblem(
			http.StatusBadRequest, "MANDATORY_IE_MISSING",
			"UE_MOBILITY requires tgtUe.supis (Target of Analytics Reporting, "+
				"TS 23.288 clause 6.7.1)")
	}
	// for each User imsi, request the engine to get location.
	for _, supi := range eventSub.TgtUe.Supis {
		var ueMobility UeMobility
		var err error
		ueMobility, err = requestUeMobEngine(
			eventSub,
			supi,
			config.Engine.Uri+config.Routes.UeMob,
		)
		if err != nil {
			return ueMobList, err
		}
		// TODO - we need to add supi to the response ueMobInfo.Supi = supi
		ueMobList = append(ueMobList, ueMobility)
	}
	return ueMobList, nil
}

// ------------------------------------------------------------------------------
// getAbnorBehavrsData - Get list Ue Communication
func getAbnormalBehaviourNotifData(
	eventSub EventSubscription,
) ([]AbnormalBehaviour, error) {
	log.Printf("Getting Abnormal Behaviour Notification Data")
	var AbnorBehavrsList []AbnormalBehaviour
	for _, excepReq := range eventSub.ExcepRequs {
		var AbnorBehavrsInfo AbnormalBehaviour
		var err error
		switch excepReq.ExcepId {

		case EXCEPTIONID_UNEXPECTED_LARGE_RATE_FLOW:
			AbnorBehavrsInfo, err = requestAbnorBehavrsEngine(
				eventSub,
				excepReq,
				config.Engine.AdsUri+config.Routes.UnexpectedLargeRate,
			)
			if err != nil {
				return AbnorBehavrsList, err
			}

		default:
			return nil, newProblem(
				http.StatusBadRequest, "UNSUPPORTED_EVENT_FILTER",
				"Exception ID is not supported; this NWDAF serves "+
					"UNEXPECTED_LARGE_RATE_FLOW only")
		}
		AbnorBehavrsInfo.Excep = excepReq
		AbnorBehavrsList = append(AbnorBehavrsList, AbnorBehavrsInfo)
	}
	return AbnorBehavrsList, nil
}

// ------------------------------------------------------------------------------
// getNfLoadNotifData - NF load statistics, TS 23.288 clause 6.5. The clause
// 6.5.1 nfInstanceIds Analytics Filter is forwarded to the engine, which owns
// the identity of the NF it measures. An empty list is a legitimate result.
func getNfLoadNotifData(eventSub EventSubscription) ([]NfLoadLevelInformation, error) {
	log.Printf("Getting NF Load Notification Data")
	return requestNfLoadEngine(
		eventSub,
		config.Engine.Uri+config.Routes.NfLoad,
	)
}

// ------------------------------------------------------------------------------
// getDnPerformanceNotifData - Get list of DnPerfInfo (TS 23.288 clause 6.14)
func getDnPerformanceNotifData(eventSub EventSubscription) ([]DnPerfInfo, error) {
	log.Printf("Getting DN Performance Notification Data")
	return requestDnPerformanceEngine(
		eventSub,
		config.Engine.Uri+config.Routes.DnPerformance,
	)
}

// ------------------------------------------------------------------------------
// getQosSustainabilityNotifData - Get list of QosSustainabilityInfo
func getQosSustainabilityNotifData(eventSub EventSubscription) ([]QosSustainabilityInfo, error) {
	log.Printf("Getting Qos Sustainability Notification Data")
	var qosSustainList []QosSustainabilityInfo
	qosSustainInfo, err := requestQosSustainabilityEngine(
		eventSub,
		config.Engine.Uri+config.Routes.QosSustainability,
	)
	if err != nil {
		return qosSustainList, err
	}
	qosSustainList = append(qosSustainList, qosSustainInfo)
	return qosSustainList, nil
}

// ------------------------------------------------------------------------------
// notifyClient - bounded HTTP client for outbound notifications.
//
// http.Post uses http.DefaultClient, which has NO timeout. One consumer that
// accepted the TCP connection and never replied blocked that subscription's
// delivery goroutine forever. CLASSIFICATION: OAI implementation gap.
var notifyClient = &http.Client{Timeout: 10 * time.Second}

// ------------------------------------------------------------------------------
// sendNotification - Nnwdaf_EventsSubscription_Notify (TS 23.288 clause
// 6.1.1.1 step 2).
//
// THE PAYLOAD IS THE POINT OF THIS FUNCTION.
//
// TS 29.520 clause 4.2.2.4.2 is normative: the request body "shall include ... a
// description of the notified event as eventNotifications attribute ... and an
// event subscription Id as subscriptionId attribute", and the OpenAPI marks
// subscriptionId `required`.
//
// This NWDAF previously POSTed a BARE EventNotification: no envelope, no
// subscriptionId, no notifCorrId. A specification-compliant consumer either
// fails schema validation outright or decodes an all-empty object, and can
// never correlate a notification with the subscription that caused it. The
// correct type already existed in this package as dead code with zero
// references anywhere.
// CLASSIFICATION: OAI implementation gap (was a blocker for any real NF consumer).
func sendNotification(
	ctx context.Context,
	eventNotif EventNotification,
	sub *subscription,
) error {
	payload := NnwdafEventsSubscriptionNotification{
		SubscriptionId:     sub.id,
		NotifCorrId:        sub.notifCorrId,
		EventNotifications: []EventNotification{eventNotif},
	}
	jsonStr, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, sub.notificationURI, bytes.NewBuffer(jsonStr))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := notifyClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf(
			"consumer answered HTTP %d to the notification", resp.StatusCode)
	}
	log.Printf(
		"Notified %s (subscriptionId=%s, event=%s, HTTP %d)",
		sub.notificationURI, sub.id, eventNotif.Event, resp.StatusCode)
	return nil
}

// ------------------------------------------------------------------------------
func requestNwPerfEngine(
	eventSub EventSubscription,
	enginePath string,
) (NetworkPerfInfo, error) {
	log.Printf("Reaching engine to get number of UE Info from DB")
	var engineReqData EngineReqData
	engineReqData.StartTs = eventSub.ExtraReportReq.StartTs
	engineReqData.EndTs = eventSub.ExtraReportReq.EndTs
	// for num_of_ue
	engineReqData.Tais = eventSub.NetworkArea.Tais
	// for sess_succ_ratio request
	engineReqData.Dnns = eventSub.Dnns
	engineReqData.Snssaia = eventSub.Snssaia
	// Convert the data to a JSON byte array
	engineReqJsonData, err := json.Marshal(engineReqData)
	if err != nil {
		return NetworkPerfInfo{}, err
	}
	// Create a POST request with the JSON data in the body
	req, err := http.NewRequest(
		http.MethodGet,
		enginePath,
		bytes.NewBuffer(engineReqJsonData),
	)
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
		NetworkArea:   eventSub.NetworkArea,
		AbsoluteNum:   nwPerfResp.AbsoluteNum,
		RelativeRatio: nwPerfResp.RelativeRatio,
		Confidence:    nwPerfResp.Confidence,
	}
	return nwPerfInfo, nil
}

// ------------------------------------------------------------------------------
func requestUeCommEngine(
	eventSub EventSubscription,
	enginePath string,
) (UeCommunication, error) {
	log.Printf("Reaching engine to get UE Communication Info from DB")
	var engineReqData EngineReqData
	engineReqData.StartTs = eventSub.ExtraReportReq.StartTs
	engineReqData.EndTs = eventSub.ExtraReportReq.EndTs
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
		UlVol:         ueCommResp.UlVol,
		UlVolVariance: ueCommResp.UlVolVariance,
		DlVol:         ueCommResp.DlVol,
		DlVolVariance: ueCommResp.DlVolVariance,
	}
	ueCommunication := UeCommunication{
		CommDur:  ueCommResp.CommDur,
		Ts:       eventSub.ExtraReportReq.StartTs,
		TrafChar: trafChar,
	}
	return ueCommunication, nil
}

// ------------------------------------------------------------------------------
func requestUeMobEngine(
	eventSub EventSubscription,
	supi string,
	enginePath string,
) (UeMobility, error) {
	log.Printf("Reaching engine to get UE Mobility Info from DB")
	log.Printf("Supi : %s", supi)
	var engineReqData EngineReqData
	engineReqData.StartTs = eventSub.ExtraReportReq.StartTs
	engineReqData.EndTs = eventSub.ExtraReportReq.EndTs
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
	for _, userLocation := range ueMobResp.Loc {
		locationInfo := LocationInfo{
			Loc:        userLocation,
			Ratio:      100,
			Confidence: 0,
		}
		ueMobility.LocInfos = append(ueMobility.LocInfos, locationInfo)
	}
	return ueMobility, nil
}

// ------------------------------------------------------------------------------
func requestAbnorBehavrsEngine(
	eventSub EventSubscription,
	excepReq Exception,
	enginePath string,
) (AbnormalBehaviour, error) {
	log.Printf("Reaching engine to get abnormal behaviour")
	var engineReqData EngineReqData
	// Convert the data to a JSON byte array
	engineReqJsonData, err := json.Marshal(engineReqData)
	if err != nil {
		return AbnormalBehaviour{}, err
	}
	// Create a POST request with the JSON data in the body
	req, err := http.NewRequest(
		http.MethodGet, enginePath, bytes.NewBuffer(engineReqJsonData))
	if err != nil {
		return AbnormalBehaviour{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	// Send the request and print the response body
	resp, err := engineClient().Do(req)
	if err != nil {
		return AbnormalBehaviour{}, err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	var abnorBehavrsResp AbnorBehavrsResp
	err = json.Unmarshal(body, &abnorBehavrsResp)
	if err != nil {
		return AbnormalBehaviour{}, err
	}
	log.Println("unexpected_large_rate_flow probability is: ",
		float64(abnorBehavrsResp.Ratio)/float64(100))
	abnormalBehaviour := AbnormalBehaviour{
		Ratio: abnorBehavrsResp.Ratio,
	}
	return abnormalBehaviour, nil
}

// ------------------------------------------------------------------------------
// getTrafficSteeringNotifData - Get list of TrafficSteeringInfo (currently just the
// engine's configured UPF).
func getTrafficSteeringNotifData() ([]TrafficSteeringInfo, error) {
	log.Printf("Getting Traffic Steering Notification Data")
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
	eventSub EventSubscription,
	enginePath string,
) ([]NfLoadLevelInformation, error) {
	log.Printf("Reaching engine to get NF Load Info from DB")
	var engineReqData EngineReqData
	engineReqData.StartTs = eventSub.ExtraReportReq.StartTs
	engineReqData.EndTs = eventSub.ExtraReportReq.EndTs
	engineReqData.NfInstanceIds = eventSub.NfInstanceIds
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
		nfLoadList = append(nfLoadList, NfLoadLevelInformation{
			NfType:             NfType(r.NfType),
			NfInstanceId:       r.NfInstanceId,
			NfCpuUsage:         r.NfCpuUsage,
			NfMemoryUsage:      r.NfMemoryUsage,
			NfLoadLevelAverage: r.NfLoadLevelAverage,
			NfLoadLevelpeak:    r.NfLoadLevelpeak,
			// Confidence deliberately unset: TS 23.288 Table 6.5.3-1
			// (NF load statistics) defines no such attribute.
		})
	}
	return nfLoadList, nil
}

// ------------------------------------------------------------------------------
// requestDnPerformanceEngine - ask the engine and map its answer onto the 3GPP
// DnPerfInfo model of TS 23.288 Table 6.14.3-1. Mirrors the analytics NBI's
// function of the same name; the two differ only in where the filters come from
// (an EventSubscription here, an EventFilter there).
func requestDnPerformanceEngine(
	eventSub EventSubscription,
	enginePath string,
) ([]DnPerfInfo, error) {
	log.Printf("Reaching engine to get DN Performance Info from DB")
	var engineReqData EngineReqData
	engineReqData.StartTs = eventSub.ExtraReportReq.StartTs
	engineReqData.EndTs = eventSub.ExtraReportReq.EndTs
	engineReqData.Dnais = eventSub.Dnais
	engineReqData.Dnns = eventSub.Dnns
	engineReqData.Snssaia = eventSub.Snssaia
	engineReqData.AppIds = eventSub.AppIds
	engineReqData.UpfId = eventSub.UpfId
	engineReqData.OffsetPeriod = eventSub.ExtraReportReq.OffsetPeriod
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
			dnPerf := DnPerf{
				UpfId: p.UpfId,
				Dnai:  p.Dnai,
				PerfData: PerfData{
					AvgTrafficRate: p.PerfData.AvgTrafficRate,
					MaxTrafficRate: p.PerfData.MaxTrafficRate,
					// AvePacketDelay / MaxPacketDelay / AvgPacketLossRate stay
					// at their zero value so omitempty keeps them off the wire.
				},
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
			// AppId deliberately unset - see the analytics NBI.
			// Confidence is set only when the engine predicted.
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
func requestQosSustainabilityEngine(
	eventSub EventSubscription,
	enginePath string,
) (QosSustainabilityInfo, error) {
	log.Printf("Reaching engine to get Qos Sustainability Info from DB")
	var engineReqData EngineReqData
	engineReqData.StartTs = eventSub.ExtraReportReq.StartTs
	engineReqData.EndTs = eventSub.ExtraReportReq.EndTs
	engineReqData.Snssaia = eventSub.Snssaia
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
		StartTs:       eventSub.ExtraReportReq.StartTs,
		EndTs:         eventSub.ExtraReportReq.EndTs,
		RanUeThrouThd: qosSustainResp.RanUeThrouThd,
		// Confidence deliberately unset - TS 23.288 Table 6.9.3-1
		// ("QoS Sustainability" statistics) defines no Confidence attribute.
	}
	// See the analytics NBI: only set AreaInfo when one was actually supplied.
	if len(eventSub.NetworkArea.Tais) > 0 || len(eventSub.NetworkArea.Ecgis) > 0 ||
		len(eventSub.NetworkArea.Ncgis) > 0 || len(eventSub.NetworkArea.GRanNodeIds) > 0 {
		area := eventSub.NetworkArea
		qosSustainInfo.AreaInfo = &area
	}
	if len(eventSub.Snssaia) > 0 {
		qosSustainInfo.Snssai = &eventSub.Snssaia[0]
	}
	return qosSustainInfo, nil
}
