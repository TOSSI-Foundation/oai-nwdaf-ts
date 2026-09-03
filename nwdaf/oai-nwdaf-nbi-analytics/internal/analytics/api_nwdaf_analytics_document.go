/*
 * SPDX-License-Identifier: LicenseRef-CSSL-1.0
 */

/*
 * Routes and config of the analytics nbi service.
 */

package analytics

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/kelseyhightower/envconfig"
)

var config AnalyticsConfig

// ------------------------------------------------------------------------------
// Type of EngineConfig structure
type AnalyticsConfig struct {
	Routes struct {
		NumOfUe           string `envconfig:"ENGINE_NUM_OF_UE_ROUTE"`
		SessSuccRatio     string `envconfig:"ENGINE_SESS_SUCC_RATIO_ROUTE"`
		UeComm            string `envconfig:"ENGINE_UE_COMMUNICATION_ROUTE"`
		UeMob             string `envconfig:"ENGINE_UE_MOBILITY_ROUTE"`
		NfLoad            string `envconfig:"ENGINE_NF_LOAD_ROUTE"`
		QosSustainability string `envconfig:"ENGINE_QOS_SUSTAINABILITY_ROUTE"`
		TrafficSteering   string `envconfig:"ENGINE_TRAFFIC_STEERING_ROUTE"`
		DnPerformance     string `envconfig:"ENGINE_DN_PERFORMANCE_ROUTE" default:"/dn_performance"`
	}
	Engine struct {
		Uri                string `envconfig:"ENGINE_URI"`
		TrafficSteeringUri string `envconfig:"ENGINE_TRAFFIC_STEERING_URI"`
	}
}

// ------------------------------------------------------------------------------
type NWDAFAnalyticsDocumentApiController struct {
	service      NWDAFAnalyticsDocumentApiServicer
	errorHandler ErrorHandler
}

// NWDAFAnalyticsDocumentApiOption for how the controller is set up.
type NWDAFAnalyticsDocumentApiOption func(*NWDAFAnalyticsDocumentApiController)

// ------------------------------------------------------------------------------
// InitConfig - Initialize global variables (config)
func InitConfig() {
	err := envconfig.Process("", &config)
	if err != nil {
		log.Fatal(err.Error())
	}
	// NRF registration parameters (TS 23.288 clause 5.1/5.2). Kept in a
	// separate struct so an unset NRF_URI leaves the NWDAF behaving exactly as
	// it did before, rather than failing to start.
	err = envconfig.Process("", &nrfConfig)
	if err != nil {
		log.Fatal(err.Error())
	}
}

// ------------------------------------------------------------------------------
// WithNWDAFAnalyticsDocumentApiErrorHandler inject ErrorHandler into controller
func WithNWDAFAnalyticsDocumentApiErrorHandler(
	h ErrorHandler,
) NWDAFAnalyticsDocumentApiOption {
	return func(c *NWDAFAnalyticsDocumentApiController) {
		c.errorHandler = h
	}
}

// ------------------------------------------------------------------------------
// NewNWDAFAnalyticsDocumentApiController creates a default api controller
func NewNWDAFAnalyticsDocumentApiController(
	s NWDAFAnalyticsDocumentApiServicer,
	opts ...NWDAFAnalyticsDocumentApiOption,
) Router {
	controller := &NWDAFAnalyticsDocumentApiController{
		service:      s,
		errorHandler: DefaultErrorHandler,
	}
	for _, opt := range opts {
		opt(controller)
	}
	return controller
}

// ------------------------------------------------------------------------------
// Routes returns all the api routes for the NWDAFAnalyticsDocumentApiController
func (c *NWDAFAnalyticsDocumentApiController) Routes() Routes {
	return Routes{
		{
			"GetNWDAFAnalytics",
			strings.ToUpper("Get"),
			"/nnwdaf-analyticsinfo/v1/analytics",
			c.GetNWDAFAnalytics,
		},
	}
}

// ------------------------------------------------------------------------------
// GetNWDAFAnalytics - Read a NWDAF Analytics
func (c *NWDAFAnalyticsDocumentApiController) GetNWDAFAnalytics(
	w http.ResponseWriter,
	r *http.Request,
) {
	log.Printf("Getting NWDAF Analytics")
	query := r.URL.Query()
	eventIdParam := query.Get("event-id")

	// The return values of these three Unmarshal calls used to be DISCARDED, so
	// a malformed ana-req / event-filter / tgt-ue silently became a zero value
	// and the NWDAF answered a request it had not understood - e.g. a broken
	// ana-req turned into "no Analytics target period" and the handler applied
	// its own default window. TS 29.500 clause 5.2.7.2 requires a syntactically
	// incorrect request to be rejected with 400 and ProblemDetails.
	// CLASSIFICATION: OAI implementation gap.
	var anaReq EventReportingRequirement
	if v := query.Get("ana-req"); v != "" {
		if err := json.Unmarshal([]byte(v), &anaReq); err != nil {
			c.errorHandler(w, r, newProblem(
				http.StatusBadRequest, "MANDATORY_IE_INCORRECT",
				"ana-req is not valid JSON: "+err.Error()), nil)
			return
		}
	}
	var eventFilter EventFilter
	if v := query.Get("event-filter"); v != "" {
		if err := json.Unmarshal([]byte(v), &eventFilter); err != nil {
			c.errorHandler(w, r, newProblem(
				http.StatusBadRequest, "MANDATORY_IE_INCORRECT",
				"event-filter is not valid JSON: "+err.Error()), nil)
			return
		}
	}
	var tgtUe TargetUeInformation
	if v := query.Get("tgt-ue"); v != "" {
		if err := json.Unmarshal([]byte(v), &tgtUe); err != nil {
			c.errorHandler(w, r, newProblem(
				http.StatusBadRequest, "MANDATORY_IE_INCORRECT",
				"tgt-ue is not valid JSON: "+err.Error()), nil)
			return
		}
	}
	supportedFeaturesParam := query.Get("supported-features")

	result, err := c.service.GetNWDAFAnalytics(
		r.Context(),
		EventIdAnyOf(eventIdParam),
		anaReq,
		eventFilter,
		supportedFeaturesParam,
		tgtUe,
	)
	// If an error occurred, encode the error with the status code
	if err != nil {
		c.errorHandler(w, r, err, &result)
		return
	}
	// TS 29.520, GET /analytics: "204 - No Content. The requested NWDAF
	// Analytics data does not exist." A 204 carries no body by definition
	// (RFC 9110 clause 15.3.5), so it must not go through EncodeJSONResponse.
	if result.Code == http.StatusNoContent {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// If no error, encode the body and the result code
	EncodeJSONResponse(result.Body, &result.Code, w)
}
