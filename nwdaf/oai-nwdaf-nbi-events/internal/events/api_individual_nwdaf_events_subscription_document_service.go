/*
 * SPDX-License-Identifier: LicenseRef-CSSL-1.0
 */

/*
 * Functions of the events nbi service (delete, update subscriptions).
 */

package events

import (
	"context"
	"log"
	"net/http"
)

// IndividualNWDAFEventsSubscriptionDocumentApiService is a service that implements the logic for the IndividualNWDAFEventsSubscriptionDocumentApiServicer
// This service should implement the business logic for every endpoint for the IndividualNWDAFEventsSubscriptionDocumentApi API.
// Include any external packages or services that will be required by this service.
type IndividualNWDAFEventsSubscriptionDocumentApiService struct {
}

// NewIndividualNWDAFEventsSubscriptionDocumentApiService creates a default api service
func NewIndividualNWDAFEventsSubscriptionDocumentApiService() IndividualNWDAFEventsSubscriptionDocumentApiServicer {
	return &IndividualNWDAFEventsSubscriptionDocumentApiService{}
}

// DeleteNWDAFEventsSubscription - Nnwdaf_EventsSubscription_Unsubscribe
// (TS 23.288 clause 6.1.1.1, TS 29.520 DELETE /subscriptions/{subscriptionId}).
func (s *IndividualNWDAFEventsSubscriptionDocumentApiService) DeleteNWDAFEventsSubscription(
	ctx context.Context,
	subscriptionId string,
) (ImplResponse, error) {
	if subscriptions.remove(subscriptionId) {
		log.Printf(
			"Deleted subscription %s (%d live subscriptions remain)",
			subscriptionId, subscriptions.count())
		return Response(http.StatusNoContent, nil), nil
	}
	// An unknown subscription id means the Individual NWDAF Events Subscription
	// RESOURCE does not exist, which is 404 - not 400. Answering 400 told the
	// consumer its request was malformed and hid the fact that its subscription
	// had already been lost (e.g. across an nbi-events restart).
	// CLASSIFICATION: OAI implementation gap.
	return ImplResponse{}, newProblem(
		http.StatusNotFound, "SUBSCRIPTION_NOT_FOUND",
		"no Individual NWDAF Events Subscription with id "+subscriptionId)
}

// UpdateNWDAFEventsSubscription - subscription modification
// (TS 23.288 clause 6.1.1.1: "If the service invocation is for a subscription
// modification, the NF service consumer includes an identifier (Subscription
// Correlation ID) to be modified"; TS 29.520 PUT /subscriptions/{subscriptionId}).
//
// NOT IMPLEMENTED. Reported as 501 with ProblemDetails so a consumer learns it
// immediately rather than assuming its modification took effect.
// CLASSIFICATION: Known non-compliance, declared. Modification is not needed by
// any consumer in this deployment: the SMF uses Nnwdaf_AnalyticsInfo
// (request/response, no subscription at all) and the traffic-steering
// controller creates a single fixed subscription at startup.
func (s *IndividualNWDAFEventsSubscriptionDocumentApiService) UpdateNWDAFEventsSubscription(
	ctx context.Context,
	subscriptionId string,
	nnwdafEventsSubscription NnwdafEventsSubscription,
) (ImplResponse, error) {
	if _, ok := subscriptions.get(subscriptionId); !ok {
		return ImplResponse{}, newProblem(
			http.StatusNotFound, "SUBSCRIPTION_NOT_FOUND",
			"no Individual NWDAF Events Subscription with id "+subscriptionId)
	}
	return ImplResponse{}, newProblem(
		http.StatusNotImplemented, "NOT_IMPLEMENTED",
		"subscription modification (TS 29.520 PUT /subscriptions/{subscriptionId}) "+
			"is not implemented by this NWDAF; delete the subscription and "+
			"create a new one")
}
