/*
 * SPDX-License-Identifier: LicenseRef-CSSL-1.0
 */

/*
 * This is main file of the oai-nwdaf-nbi-events HTTP server.
 */

package main

import (
	"log"
	"net/http"
	"time"

	"github.com/joho/godotenv"
	"github.com/kelseyhightower/envconfig"
	"gitlab.eurecom.fr/oai/cn5g/oai-cn5g-nwdaf/components/oai-nwdaf-nbi-events/internal/events"
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
	events.InitConfig()
	IndividualNWDAFEventSubscriptionTransferDocumentApiService := events.NewIndividualNWDAFEventSubscriptionTransferDocumentApiService()
	IndividualNWDAFEventSubscriptionTransferDocumentApiController := events.NewIndividualNWDAFEventSubscriptionTransferDocumentApiController(
		IndividualNWDAFEventSubscriptionTransferDocumentApiService,
		events.WithIndividualNWDAFEventSubscriptionTransferDocumentApiErrorHandler(events.ProblemDetailsErrorHandler),
	)
	IndividualNWDAFEventsSubscriptionDocumentApiService := events.NewIndividualNWDAFEventsSubscriptionDocumentApiService()
	IndividualNWDAFEventsSubscriptionDocumentApiController := events.NewIndividualNWDAFEventsSubscriptionDocumentApiController(
		IndividualNWDAFEventsSubscriptionDocumentApiService,
		events.WithIndividualNWDAFEventsSubscriptionDocumentApiErrorHandler(events.ProblemDetailsErrorHandler),
	)
	NWDAFEventSubscriptionTransfersCollectionApiService := events.NewNWDAFEventSubscriptionTransfersCollectionApiService()
	NWDAFEventSubscriptionTransfersCollectionApiController := events.NewNWDAFEventSubscriptionTransfersCollectionApiController(
		NWDAFEventSubscriptionTransfersCollectionApiService,
		events.WithNWDAFEventSubscriptionTransfersCollectionApiErrorHandler(events.ProblemDetailsErrorHandler),
	)
	NWDAFEventsSubscriptionsCollectionApiService := events.NewNWDAFEventsSubscriptionsCollectionApiService()
	NWDAFEventsSubscriptionsCollectionApiController := events.NewNWDAFEventsSubscriptionsCollectionApiController(
		NWDAFEventsSubscriptionsCollectionApiService,
		events.WithNWDAFEventsSubscriptionsCollectionApiErrorHandler(events.ProblemDetailsErrorHandler),
	)
	router := events.NewRouter(
		IndividualNWDAFEventSubscriptionTransferDocumentApiController,
		IndividualNWDAFEventsSubscriptionDocumentApiController,
		NWDAFEventSubscriptionTransfersCollectionApiController,
		NWDAFEventsSubscriptionsCollectionApiController,
	)
	// TS 29.520 clause 5.2.2.1: "HTTP/2 [...] shall be used" for the
	// Nnwdaf_EventsSubscription service API. This server was a plain net/http
	// ListenAndServe, i.e. HTTP/1.1 cleartext only - the same gap already
	// closed on oai-nwdaf-nbi-analytics. An OAI NF consumer configured with
	// http_version: 2 sends prior-knowledge HTTP/2 and an HTTP/1.1-only server
	// never answers it, so the consumer hangs.
	//
	// h2c.NewHandler serves prior-knowledge HTTP/2 AND plain HTTP/1.1 on the
	// same port, so the existing Python consumers keep working unchanged.
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
