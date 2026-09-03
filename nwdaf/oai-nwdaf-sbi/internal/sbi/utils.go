/*
 * SPDX-License-Identifier: LicenseRef-CSSL-1.0
 */

/*
 * This file contains utils functions.
 */

package sbi

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/kelseyhightower/envconfig"
	amf_client "gitlab.eurecom.fr/oai/cn5g/oai-cn5g-nwdaf/components/oai-nwdaf-sbi/internal/amfclient"
	smf_client "gitlab.eurecom.fr/oai/cn5g/oai-cn5g-nwdaf/components/oai-nwdaf-sbi/internal/smfclient"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"golang.org/x/net/http2"
)

// ------------------------------------------------------------------------------
// subscriptionClient - HTTP client used for the AMF/SMF event-exposure
// subscriptions, honouring AMF_HTTP_VERSION / SMF_HTTP_VERSION.
//
// Two problems it fixes, both hit when running against a control plane
// configured with `http_version: 2`:
//
//  1. The generated OpenAPI clients default to net/http, which speaks HTTP/1.1.
//     An OAI NF running its nghttp2 SBI server accepts h2c ONLY and simply never
//     answers an HTTP/1.1 request, so the call hangs. AMF_HTTP_VERSION and
//     SMF_HTTP_VERSION were already accepted as env vars (and set in every
//     compose file) but no code path ever read them.
//  2. The default client has NO timeout, and InitConfig() subscribes to the AMF
//     first, synchronously. A non-answering AMF therefore blocked the SMF
//     subscription AND ListenAndServe() forever - the sbi looked "Up" while
//     collecting nothing and serving nothing.
func subscriptionClient(httpVersion string) *http.Client {
	if httpVersion != "2" {
		return &http.Client{Timeout: 10 * time.Second}
	}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http2.Transport{
			// h2c: prior-knowledge HTTP/2 over cleartext TCP.
			AllowHTTP: true,
			DialTLSContext: func(
				ctx context.Context, network, addr string, _ *tls.Config,
			) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		},
	}
}

// ------------------------------------------------------------------------------
// InitConfig - Initialize global variables (cfg and mongoClient) and subscribe to AMF and SMF
func InitConfig() {
	err := envconfig.Process("", &config)
	if err != nil {
		log.Fatal(err.Error())
	}
	clientOptions := options.Client().ApplyURI(config.Database.Uri)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, clientOptions)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Connected to MongoDB.")
	mongoClient = client
	// Event-exposure subscriptions are managed by subscription_manager.go:
	// delete-any-stale, create, retry with backoff, poll liveness, unsubscribe
	// on shutdown. They used to be created here, once, synchronously, with no
	// retry and no recovery - see the header of subscription_manager.go.
	InitSubscriptionManager()
}

// ------------------------------------------------------------------------------
func amfEventSubscription(
	amfEventNotifyUri string,
	amfNotifyCorrelationId string,
	amfNfId string,
) (string, error) {
	// Store all AMF event types
	var amfEvents []amf_client.AmfEvent
	for _, amfEventTypeAnyOf := range amf_client.AllowedAmfEventTypeAnyOfEnumValues {
		amfEvents = append(amfEvents, *amf_client.NewAmfEvent(amfEventTypeAnyOf))
	}
	// Subscribe to all AMF event types
	amfCreateEventSubscription := *amf_client.NewAmfCreateEventSubscription(
		*amf_client.NewAmfEventSubscription(
			amfEvents,
			amfEventNotifyUri,
			amfNotifyCorrelationId,
			amfNfId,
		),
	)
	configuration := amf_client.NewConfiguration()
	configuration.HTTPClient = subscriptionClient(config.Amf.HttpVersion)
	amfApiClient := amf_client.NewAPIClient(configuration)
	resp, r, err := amfApiClient.SubscriptionsCollectionCollectionApi.CreateSubscription(
		context.Background()).AmfCreateEventSubscription(amfCreateEventSubscription).Execute()
	if err != nil {
		return "", err
	}
	if r == nil || r.StatusCode != http.StatusCreated {
		code := 0
		if r != nil {
			code = r.StatusCode
		}
		return "", fmt.Errorf(
			"AMF answered HTTP %d to Namf_EventExposure_Subscribe "+
				"(TS 29.518 clause 5.2.2.3.1 expects 201)", code)
	}
	log.Printf(
		"Response from `SubscriptionsCollectionCollectionApi.CreateSubscription`: %v\n",
		resp,
	)
	return resourceUriFromResponse(
		peerAmf, r, config.Amf.IpAddr+config.Amf.SubRoute+"/subscriptions"), nil
}

// ------------------------------------------------------------------------------
func smfEventSubscription(smfEventNotifyUri string, smfNfId string) (string, error) {

	// Store all SMF event types
	var smfEventSubs []smf_client.EventSubscription
	smfEventSubs = append(smfEventSubs,
		*smf_client.NewEventSubscription(smf_client.SMFEVENTANYOF_PDU_SES_EST),
	)
	smfEventSubs = append(smfEventSubs,
		*smf_client.NewEventSubscription(smf_client.SMFEVENTANYOF_UE_IP_CH),
	)
	smfEventSubs = append(smfEventSubs,
		*smf_client.NewEventSubscription(smf_client.SMFEVENTANYOF_PLMN_CH),
	)
	smfEventSubs = append(smfEventSubs,
		*smf_client.NewEventSubscription(smf_client.SMFEVENTANYOF_DDDS),
	)
	smfEventSubs = append(smfEventSubs,
		*smf_client.NewEventSubscription(smf_client.SMFEVENTANYOF_PDU_SES_REL),
	)
	smfEventSubs = append(smfEventSubs,
		*smf_client.NewEventSubscription(smf_client.SMFEVENTANYOF_QOS_MON),
	)
	// TS 29.508 UP_PATH_CH. Carries the DNAI each PDU session's N6 path is
	// bound to, which TS 23.288 Table 6.4.2-2 names as SMF-sourced input to DN
	// Performance analytics. The OAI SMF could always PARSE this subscription
	// but only started notifying it with the DN_PERFORMANCE work - against an
	// older SMF the subscription is simply accepted and never fires, so adding
	// it here is backwards-compatible.
	smfEventSubs = append(smfEventSubs,
		*smf_client.NewEventSubscription(smf_client.SMFEVENTANYOF_UP_PATH_CH),
	)
	// Subscribe to all SMF event types
	nsmfEventExposure := *smf_client.NewNsmfEventExposure(
		smfNfId,
		smfEventNotifyUri,
		smfEventSubs,
	)
	configuration := smf_client.NewConfiguration()
	configuration.HTTPClient = subscriptionClient(config.Smf.HttpVersion)
	smfApiClient := smf_client.NewAPIClient(configuration)
	resp, r, err := smfApiClient.SubscriptionsCollectionApi.CreateIndividualSubcription(
		context.Background()).NsmfEventExposure(nsmfEventExposure).Execute()
	if err != nil {
		return "", err
	}
	if r == nil || r.StatusCode != http.StatusCreated {
		code := 0
		if r != nil {
			code = r.StatusCode
		}
		return "", fmt.Errorf(
			"SMF answered HTTP %d to Nsmf_EventExposure_Subscribe "+
				"(TS 29.508 clause 5.2.2.2.1 expects 201)", code)
	}
	log.Printf(
		"Response from `SubscriptionsCollectionApi.CreateIndividualSubcription`: %v\n",
		resp,
	)
	return resourceUriFromResponse(
		peerSmf, r, config.Smf.IpAddr+config.Smf.SubRoute+"/subscriptions"), nil
}
