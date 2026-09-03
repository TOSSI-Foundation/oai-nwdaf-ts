/*
 * SPDX-License-Identifier: LicenseRef-CSSL-1.0
 */

/*
 * Nnwdaf_EventsSubscription subscription state.
 *
 * WHAT WAS HERE BEFORE
 * -----------------------------------------------------------------------------
 *     var subscriptionTable = make(map[string]chan string)
 *
 * A bare map from subscription id to a cancel channel, written from HTTP
 * handlers with NO synchronisation. Two problems, both real:
 *
 *  1. Concurrent Subscribe and Unsubscribe are concurrent map write/delete,
 *     which the Go runtime turns into a fatal "concurrent map writes" panic.
 *     That would kill the process and therefore EVERY subscription at once,
 *     including the traffic-steering controller's.
 *  2. It stored only the cancel channel, so nothing else about the
 *     subscription survived - in particular not the subscriptionId or the
 *     notifCorrId, which TS 29.520 requires the NWDAF to send back in every
 *     notification (see notifyPayload in
 *     api_nwdaf_events_subscriptions_collection_service.go).
 *
 * CLASSIFICATION: OAI implementation gap.
 *
 * WHAT IS STILL MISSING - KNOWN NON-COMPLIANCE
 * -----------------------------------------------------------------------------
 * This registry is IN-MEMORY. A restart of oai-nwdaf-nbi-events still drops
 * every subscription and every delivery goroutine, and the consumer is never
 * told. TS 23.288 clause 6.1.1.1 gives the producer a Termination Request for
 * ending a subscription deliberately; it says nothing about surviving a crash,
 * so this is a robustness gap rather than a clause violation - but a consumer
 * WILL silently stop receiving data. Persisting to the MongoDB already in the
 * stack is Tier 2 work and is deliberately not done here.
 */

package events

import "sync"

// ------------------------------------------------------------------------------
// subscription - everything the NWDAF must remember about one Individual NWDAF
// Events Subscription resource.
type subscription struct {
	// id is the {subscriptionId} of the resource URI returned in Location.
	id string
	// notifCorrId is the consumer's Notification Correlation ID, echoed in
	// every notification (TS 23.288 clause 6.1.3).
	notifCorrId string
	// notificationURI is the consumer's Notification Target Address.
	notificationURI string
	// cancel is closed to stop every delivery goroutine of this subscription.
	cancel chan string
}

// ------------------------------------------------------------------------------
type subscriptionRegistry struct {
	mu   sync.RWMutex
	subs map[string]*subscription
}

var subscriptions = subscriptionRegistry{subs: make(map[string]*subscription)}

// add - register a new subscription.
func (r *subscriptionRegistry) add(s *subscription) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.subs[s.id] = s
}

// get - look one up. ok is false when the resource does not exist, which the
// caller must map onto 404 rather than 400.
func (r *subscriptionRegistry) get(id string) (*subscription, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.subs[id]
	return s, ok
}

// remove - drop the subscription and stop its delivery goroutines. Returns
// false if the subscription id was not known.
func (r *subscriptionRegistry) remove(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.subs[id]
	if !ok {
		return false
	}
	close(s.cancel)
	delete(r.subs, id)
	return true
}

// count - number of live subscriptions, for logging.
func (r *subscriptionRegistry) count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.subs)
}
