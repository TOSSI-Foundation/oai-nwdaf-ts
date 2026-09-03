/*
 * SPDX-License-Identifier: LicenseRef-CSSL-1.0
 */

/*
 * This is main file of the oai-nwdaf-nbi-analytics HTTP server.
 */

package main

import (
	"log"
	"net/http"
	"time"

	"github.com/joho/godotenv"
	"github.com/kelseyhightower/envconfig"
	"gitlab.eurecom.fr/oai/cn5g/oai-cn5g-nwdaf/components/oai-nwdaf-nbi-analytics/internal/analytics"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

type MainConfig struct {
	Server struct {
		Addr string `envconfig:"SERVER_ADDR"`
	}
}

func main() {
	// load the environment variables from the file .env
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Error loading .env file")
	}
	var config MainConfig
	err = envconfig.Process("", &config)
	if err != nil {
		log.Fatal(err.Error())
	}
	// Initialize internal package
	analytics.InitConfig()
	// TS 23.288 clause 5.2: a consumer NF discovers this NWDAF through the
	// NRF. No-op unless NRF_URI is configured.
	analytics.InitNrfRegistration()
	// Create router
	NWDAFAnalyticsDocumentApiService := analytics.NewNWDAFAnalyticsDocumentApiService()
	NWDAFAnalyticsDocumentApiController := analytics.NewNWDAFAnalyticsDocumentApiController(
		NWDAFAnalyticsDocumentApiService,
		// TS 29.520 declares application/problem+json ProblemDetails for every
		// error response of GET /analytics. The generated DefaultErrorHandler
		// discards the ProblemDetails body and writes a bare JSON string.
		analytics.WithNWDAFAnalyticsDocumentApiErrorHandler(
			analytics.ProblemDetailsErrorHandler),
	)
	NWDAFContextDocumentApiService := analytics.NewNWDAFContextDocumentApiService()
	NWDAFContextDocumentApiController := analytics.NewNWDAFContextDocumentApiController(
		NWDAFContextDocumentApiService,
		analytics.WithNWDAFContextDocumentApiErrorHandler(
			analytics.ProblemDetailsErrorHandler),
	)
	router := analytics.NewRouter(
		NWDAFAnalyticsDocumentApiController,
		NWDAFContextDocumentApiController,
	)
	// TS 29.520 clause 5.1.2.1: "HTTP/2 [...] shall be used" for the Nnwdaf
	// service APIs. This server was a plain net/http ListenAndServe, i.e.
	// HTTP/1.1 cleartext only.
	//
	// This is not merely a compliance point - it blocks real NF consumers.
	// The OAI NFs share ONE http_client singleton whose HTTP version comes
	// from the deployment config (http_version: 2 here), and cpr then sends
	// VERSION_2_0_PRIOR_KNOWLEDGE. An HTTP/1.1-only server never answers such
	// a request, so an SMF consuming Nnwdaf_AnalyticsInfo would simply hang -
	// the same trap already documented for the PCF in
	// scripts/nwdaf_steering_controller.py and for the AMF/SMF in
	// oai-nwdaf-sbi/internal/sbi/utils.go.
	//
	// h2c.NewHandler serves prior-knowledge HTTP/2 AND plain HTTP/1.1 on the
	// same port, so every existing consumer (the CLI, the Python controller)
	// keeps working unchanged while NF consumers get the transport the spec
	// mandates.
	h2s := &http2.Server{}
	server := &http.Server{
		Addr:         config.Server.Addr,
		Handler:      h2c.NewHandler(router, h2s),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
	log.Printf("Server listening at %s", config.Server.Addr)
	log.Fatal(server.ListenAndServe())
}
