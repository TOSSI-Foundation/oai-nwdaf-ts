/*
 * SPDX-License-Identifier: LicenseRef-CSSL-1.0
 */

/*
 * Data-collection subscription lifecycle (TS 23.288 clause 6.2.2).
 *
 * WHAT THE SPECIFICATION REQUIRES
 * -----------------------------------------------------------------------------
 * TS 23.288 clause 6.2.2.5: "The NWDAF shall subscribe (and unsubscribe) to the
 * Event exposure service from NF(s) reusing the framework defined in clause 4.15
 * of TS 23.502". Clause 6.2.2.1 Table 6.2.2.1-1 names the services used here:
 * Namf_EventExposure (TS 23.502 clause 5.2.2.3) and Nsmf_EventExposure
 * (clause 5.2.8.3). The stage-3 resources are TS 29.518 clause 5.2.2.3 for the
 * AMF and TS 29.508 clause 5.2.2 for the SMF; in both, creating a subscription
 * returns 201 with a Location header naming the Individual Subscription
 * resource, and DELETE on that resource is unsubscribe.
 *
 * WHAT WAS WRONG BEFORE
 * -----------------------------------------------------------------------------
 * InitConfig() called amfEventSubscription() and smfEventSubscription() ONCE,
 * synchronously, at startup:
 *
 *   1. NO RETRY. The AMF subscription timed out at 2026-08-21 10:13:59 with
 *      "dial tcp 192.168.70.132:8080: i/o timeout" and was never attempted
 *      again. The `amf` collection was therefore never created in four days of
 *      uptime, and UE_MOBILITY plus NETWORK_PERFORMANCE had no data at all.
 *      The AMF was reachable again minutes later.
 *
 *   2. NO RECOVERY. The SMF subscription succeeded, then the SMF restarted.
 *      Event-exposure subscriptions live in the SMF's memory, so it silently
 *      forgot ours. The last stored notification is 2026-08-21 11:14:07 - the
 *      `smf` collection has been stale ever since, which in turn zeroed every
 *      SMF-derived analytic AND the custom traffic-steering feature vector.
 *
 *   3. NO UNSUBSCRIBE, so every restart of THIS component added a duplicate
 *      subscription to a still-running peer. The SMF then delivered each
 *      notification once per stale subscription and identical usage reports
 *      (same supi + seid + urseqn) were stored N times, inflating every summed
 *      rate N-fold. The Location header was discarded, so nothing could have
 *      deleted them.
 *
 * CLASSIFICATION: OAI implementation gap (all three).
 *
 * WHAT THIS FILE DOES
 * -----------------------------------------------------------------------------
 *   startup   - delete any subscription this component created in a previous
 *               life (URIs persisted in MongoDB), then create a fresh one
 *   failure   - retry with bounded exponential backoff, indefinitely, so a peer
 *               that is not up yet is picked up when it appears
 *   liveness  - periodically GET the Individual Subscription resource; a 404
 *               means the peer forgot us (typically a peer restart) and the
 *               subscription is recreated
 *   shutdown  - DELETE both subscriptions and clear the persisted state
 *
 * UNSUBSCRIBE CANNOT CURRENTLY SUCCEED - KNOWN NON-COMPLIANCE (OAI upstream)
 * -----------------------------------------------------------------------------
 * Neither peer implements the Individual Subscription resource at all:
 *
 *   SMF  smf-http2-server.cpp registers ONLY
 *        SmfEventExposureBase() + SmfEventExposurePathSubscriptions, i.e. the
 *        COLLECTION. The per-subscription path constant exists in the shared
 *        header (SmfEventExposurePathSubscriptionsSubscriptionId =
 *        "/subscriptions/:subId") and is never passed to server.handle(), so
 *        DELETE and GET on it answer 404. TS 29.508 clause 5.2.3.2 requires
 *        Nsmf_EventExposure_Unsubscribe.
 *   AMF  does not answer at all (connection times out) on
 *        /namf-evts/v1/subscriptions/{subscriptionId}. TS 29.518 clause 5.2.2.4
 *        requires Namf_EventExposure_Unsubscribe.
 *
 * Verified live 2026-08-25. The delete-then-create and liveness logic below is
 * therefore CORRECT BUT INERT against these peers: it will start working the
 * moment either NF implements the resource, and it costs nothing meanwhile.
 * The residual duplicate-subscription risk on restart is mitigated - NOT fixed -
 * by de-duplicating usage reports on their own (seid, urseqn) identity in
 * scripts/monitoring/collect_upf_metrics.py and in the engine handlers.
 *
 * WHAT IT DELIBERATELY DOES NOT DO
 * -----------------------------------------------------------------------------
 * It does not discover the AMF/SMF through the NRF. The endpoints stay
 * configured (AMF_IP_ADDR / SMF_IP_ADDR), which is how the OAI deployment wires
 * this component. TS 23.288 clause 6.2.2.4 does permit NRF-based discovery, and
 * that remains a KNOWN GAP - recorded, not silently implied. In this deployment
 * it would not currently help: the OAI AMF is failing its own NRF registration
 * with "getaddrinfo() thread failed to start / Could not resolve host: oai-nrf"
 * after thousands of consecutive attempts, so it is not discoverable at all.
 * CLASSIFICATION of that: Deployment/test-environment defect, outside the NWDAF.
 */

package sbi

import (
	"context"
	"encoding/json"
	"io/ioutil"
	"log"
	"net/http"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	peerAmf = "AMF"
	peerSmf = "SMF"

	// Collection holding the Individual Subscription resource URIs this
	// component created, so a restart can delete them before creating new ones.
	// Persisted in the NWDAF's own MongoDB, which outlives this container.
	subscriptionStateCollection = "sbi_subscriptions"

	subscribeBackoffInitial = 5 * time.Second
	subscribeBackoffMax     = 60 * time.Second
	livenessCheckInterval   = 30 * time.Second
)

// ------------------------------------------------------------------------------
// storedSubscription - one persisted Individual Subscription resource.
type storedSubscription struct {
	Peer        string `bson:"_id"`
	ResourceUri string `bson:"resourceUri"`
	CreatedAt   int64  `bson:"createdAt"`
}

// ------------------------------------------------------------------------------
// subscriptionState - live state for one peer.
type subscriptionState struct {
	mu          sync.RWMutex
	resourceUri string
	lastOk      time.Time
	// livenessProbed/livenessSupported record whether the peer actually
	// implements a readable Individual Subscription resource. See
	// probeLivenessSupport - assuming it does caused a resubscribe LOOP.
	livenessProbed    bool
	livenessSupported bool
	// peerNfInstanceId is the peer's NF Instance ID as last seen in the NRF.
	// A change means the peer restarted and forgot our subscription
	// (TS 23.288 clause 6.2.2.4 NF/NF service discovery, performed
	// "on a periodic basis").
	peerNfInstanceId string
}

var (
	amfSubscription subscriptionState
	smfSubscription subscriptionState
	managerStop     = make(chan struct{})
	managerStopOnce sync.Once
)

func stateFor(peer string) *subscriptionState {
	if peer == peerAmf {
		return &amfSubscription
	}
	return &smfSubscription
}

func (s *subscriptionState) set(uri string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resourceUri = uri
	s.lastOk = time.Now()
}

func (s *subscriptionState) get() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.resourceUri
}

// ------------------------------------------------------------------------------
// stateCollection - the MongoDB collection holding persisted subscription URIs.
func stateCollection() *mongo.Collection {
	return mongoClient.
		Database(config.Database.DbName).
		Collection(subscriptionStateCollection)
}

func loadStoredSubscription(peer string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var doc storedSubscription
	err := stateCollection().
		FindOne(ctx, bson.M{"_id": peer}).
		Decode(&doc)
	if err != nil {
		return ""
	}
	return doc.ResourceUri
}

func saveStoredSubscription(peer string, uri string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := stateCollection().UpdateByID(
		ctx, peer,
		bson.M{"$set": bson.M{
			"resourceUri": uri,
			"createdAt":   time.Now().Unix(),
		}},
		options.Update().SetUpsert(true),
	)
	if err != nil {
		log.Printf(
			"%s subscription: could not persist resource URI (%v). A restart "+
				"will not be able to delete it and may create a duplicate.",
			peer, err)
	}
}

func clearStoredSubscription(peer string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stateCollection().DeleteOne(ctx, bson.M{"_id": peer})
}

// ------------------------------------------------------------------------------
// subscriptionHttpClient - the peers' SBI servers speak cleartext HTTP/2 when
// configured with http_version: 2 (see subscriptionClient in utils.go). The
// Individual Subscription resource is addressed by the absolute URI the peer
// returned in Location, so these direct calls reuse the same transport rather
// than the generated client wrapper, which only knows the collection endpoint.
func subscriptionHttpClient(peer string) *http.Client {
	if peer == peerAmf {
		return subscriptionClient(config.Amf.HttpVersion)
	}
	return subscriptionClient(config.Smf.HttpVersion)
}

// ------------------------------------------------------------------------------
// deleteSubscriptionResource - Namf_EventExposure_Unsubscribe (TS 29.518 clause
// 5.2.2.4) / Nsmf_EventExposure_Unsubscribe (TS 29.508 clause 5.2.3.2):
// DELETE on the Individual Subscription resource.
//
// A 404 is treated as success: the goal is "this subscription no longer
// exists", and it already does not.
func deleteSubscriptionResource(peer string, uri string) bool {
	if uri == "" {
		return true
	}
	req, err := http.NewRequest(http.MethodDelete, uri, nil)
	if err != nil {
		log.Printf("%s unsubscribe: bad resource URI %q: %v", peer, uri, err)
		return false
	}
	resp, err := subscriptionHttpClient(peer).Do(req)
	if err != nil {
		log.Printf("%s unsubscribe: DELETE %s failed: %v", peer, uri, err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent ||
		resp.StatusCode == http.StatusOK ||
		resp.StatusCode == http.StatusNotFound {
		log.Printf("%s unsubscribe: DELETE %s -> HTTP %d",
			peer, uri, resp.StatusCode)
		return true
	}
	log.Printf("%s unsubscribe: DELETE %s -> unexpected HTTP %d",
		peer, uri, resp.StatusCode)
	return false
}

// ------------------------------------------------------------------------------
// subscriptionResourceExists - GET the Individual Subscription resource.
//
// Returns (exists, verifiable). verifiable is false when the peer does not
// implement a readable Individual Subscription resource, in which case the
// caller must NOT infer that the subscription is gone. TS 29.518 defines no GET
// on the AMF's Individual Subscription resource at all, so for the AMF this is
// expected to report "not verifiable" and liveness cannot be polled - a
// KNOWN GAP, stated rather than papered over with a guess.
func subscriptionResourceExists(peer string, uri string) (bool, bool) {
	if uri == "" {
		return false, true
	}
	req, err := http.NewRequest(http.MethodGet, uri, nil)
	if err != nil {
		return false, false
	}
	resp, err := subscriptionHttpClient(peer).Do(req)
	if err != nil {
		// Transport failure says nothing about the subscription itself.
		return false, false
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, true
	case http.StatusNotFound:
		return false, true
	case http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return false, false
	default:
		return false, false
	}
}

// ------------------------------------------------------------------------------
// InitSubscriptionManager - start one supervisor per peer.
//
// Called instead of the old fire-and-forget subscribe calls. Returns
// immediately: a peer that is down must not block the SBI's own HTTP server
// from starting, or the NWDAF looks "Up" while serving nothing.
func InitSubscriptionManager() {
	go superviseSubscription(peerAmf)
	go superviseSubscription(peerSmf)
}

// ------------------------------------------------------------------------------
func superviseSubscription(peer string) {
	// 1. Delete whatever this component left behind in a previous life. This is
	//    what stops restart-driven duplicate subscriptions.
	if stale := loadStoredSubscription(peer); stale != "" {
		log.Printf(
			"%s subscription: found a persisted subscription from a previous "+
				"run (%s) - deleting it before subscribing again", peer, stale)
		deleteSubscriptionResource(peer, stale)
		clearStoredSubscription(peer)
	}

	backoff := subscribeBackoffInitial
	for {
		select {
		case <-managerStop:
			return
		default:
		}

		uri := stateFor(peer).get()
		if uri == "" {
			// 2. (Re)create.
			created, err := createSubscription(peer)
			if err != nil {
				log.Printf(
					"%s subscription: create failed (%v) - retrying in %s",
					peer, err, backoff)
				if !sleepOrStop(backoff) {
					return
				}
				backoff *= 2
				if backoff > subscribeBackoffMax {
					backoff = subscribeBackoffMax
				}
				continue
			}
			backoff = subscribeBackoffInitial
			if created == "" {
				// The subscription exists on the peer but carries no
				// addressable resource URI, so it can never be deleted and
				// liveness cannot be polled. Do not retry: retrying would
				// create a second undeletable subscription every interval.
				log.Printf(
					"%s subscription: created but not addressable (no Location "+
						"header). Liveness polling and unsubscribe are "+
						"DISABLED for this peer.", peer)
				return
			}
			stateFor(peer).set(created)
			saveStoredSubscription(peer, created)
			log.Printf("%s subscription: created, resource %s", peer, created)
			probeLivenessSupport(peer, created)
			recordPeerNfInstanceId(peer)
			if !sleepOrStop(livenessCheckInterval) {
				return
			}
			continue
		}

		// 3. Liveness. A peer restart silently discards our subscription -
		//    event-exposure subscriptions are in-memory in both OAI NFs.
		if peerRestarted(peer) {
			log.Printf(
				"%s subscription: the peer's NF Instance ID changed in the "+
					"NRF, so it restarted and no longer holds our "+
					"subscription - resubscribing", peer)
			stateFor(peer).set("")
			clearStoredSubscription(peer)
			continue
		}
		if livenessPollable(peer) {
			exists, verifiable := subscriptionResourceExists(peer, uri)
			if verifiable && !exists {
				log.Printf(
					"%s subscription: resource %s no longer exists - "+
						"resubscribing", peer, uri)
				stateFor(peer).set("")
				clearStoredSubscription(peer)
				continue
			}
		}
		if !sleepOrStop(livenessCheckInterval) {
			return
		}
	}
}

func sleepOrStop(d time.Duration) bool {
	select {
	case <-managerStop:
		return false
	case <-time.After(d):
		return true
	}
}

// ------------------------------------------------------------------------------
// createSubscription - one Namf/Nsmf_EventExposure_Subscribe, returning the
// absolute Individual Subscription resource URI from the Location header.
func createSubscription(peer string) (string, error) {
	if peer == peerAmf {
		return amfEventSubscription(
			config.Server.NotifUri+config.Amf.ApiRoute,
			config.Amf.NotifCorrId,
			config.Amf.NotifId,
		)
	}
	return smfEventSubscription(
		config.Server.NotifUri+config.Smf.ApiRoute,
		config.Smf.NotifId,
	)
}

// ------------------------------------------------------------------------------
// resourceUriFromResponse - derive the Individual Subscription resource URI
// from the peer's 201 response.
//
// OAI UPSTREAM DEFECT - the Location headers these peers send are NOT valid
// resource URIs, so this has to repair them. Measured 2026-08-25:
//
//	SMF: "192.168.70.133/nsmf-event-exposure/v1/subscriptions2"
//	     no scheme, no port, and the subscription id is CONCATENATED onto
//	     "subscriptions" with no separating "/" (smf-http2-server.cpp builds
//	     SmfEventExposureBase() + SmfEventExposurePathSubscriptions + sub_id).
//	AMF: "192.168.70.132:8080/namf-evts/v1/namf-evts/2"
//	     no scheme, and the service name is repeated instead of the
//	     "/subscriptions/" path segment.
//
// TS 29.508 clause 5.2.2.2.1 and TS 29.518 clause 5.2.2.3.1 both require
// {apiRoot}/<service>/<apiVersion>/subscriptions/{subscriptionId}.
//
// Only the trailing subscription ID is trustworthy, so the URI is rebuilt from
// the CONFIGURED apiRoot plus the specification's resource structure. That is a
// repair of a peer defect, not an invented API: the path is the one the spec
// defines and the one the peer's own sbi_helper declares
// (SmfEventExposurePathSubscriptionsSubscriptionId = "/subscriptions/:subId").
func resourceUriFromResponse(peer string, resp *http.Response, apiRootBase string) string {
	if resp == nil {
		return ""
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		log.Printf(
			"%s subscription: the peer answered HTTP %d with NO Location "+
				"header. The subscription cannot be addressed and therefore "+
				"cannot be deleted. (TS 29.518 5.2.2.3.1 / TS 29.508 "+
				"5.2.2.2.1 require it.)", peer, resp.StatusCode)
		return ""
	}
	subId := subscriptionIdFromLocation(loc)
	if subId == "" {
		log.Printf(
			"%s subscription: could not extract a subscription id from the "+
				"Location header %q - unsubscribe will not be possible",
			peer, loc)
		return ""
	}
	uri := strings.TrimRight(apiRootBase, "/") + "/" + subId
	if uri != loc {
		log.Printf(
			"%s subscription: the peer's Location header %q is not a valid "+
				"resource URI (OAI upstream defect); using the "+
				"specification-correct %s instead", peer, loc, uri)
	}
	return uri
}

// ------------------------------------------------------------------------------
// subscriptionIdFromLocation - pull the subscription id out of a malformed
// Location. Handles both shapes observed:
//
//	".../subscriptions/2"  -> "2"   (spec-correct)
//	".../subscriptions2"   -> "2"   (SMF: missing separator)
//	".../namf-evts/2"      -> "2"   (AMF: wrong path segment)
func subscriptionIdFromLocation(loc string) string {
	loc = strings.TrimRight(loc, "/")
	last := loc
	if i := strings.LastIndex(loc, "/"); i >= 0 {
		last = loc[i+1:]
	}
	if last == "" {
		return ""
	}
	// "subscriptions2" -> "2"
	if idx := strings.LastIndex(last, "subscriptions"); idx >= 0 {
		trimmed := last[idx+len("subscriptions"):]
		if trimmed != "" {
			return trimmed
		}
		return ""
	}
	return last
}

// ------------------------------------------------------------------------------
// probeLivenessSupport - find out ONCE whether this peer implements a readable
// Individual Subscription resource.
//
// WHY THIS EXISTS - a real regression this code caused and now prevents.
// The first version polled GET on the resource and treated 404 as "the peer
// dropped our subscription". Neither OAI peer implements that resource at all
// (the SMF registers only the COLLECTION route; the AMF does not answer), so
// every poll returned 404, every poll resubscribed, and the supervisor created
// a NEW subscription every 30 seconds - 13 of them in six minutes, none of them
// deletable. That is strictly worse than the restart-driven duplication this
// file set out to fix.
//
// A 404 from a peer that has no such route is NOT evidence about the
// subscription. So capability is established once, right after a successful
// create, when the subscription is known to exist: only a 200 proves the
// resource is implemented, and anything else disables polling for that peer.
func probeLivenessSupport(peer string, uri string) {
	st := stateFor(peer)
	st.mu.Lock()
	already := st.livenessProbed
	st.mu.Unlock()
	if already {
		return
	}
	exists, _ := subscriptionResourceExists(peer, uri)
	st.mu.Lock()
	st.livenessProbed = true
	st.livenessSupported = exists
	st.mu.Unlock()
	if exists {
		log.Printf(
			"%s subscription: the peer implements the Individual Subscription "+
				"resource - liveness polling enabled", peer)
		return
	}
	log.Printf(
		"%s subscription: the peer does NOT implement a readable Individual "+
			"Subscription resource (a GET on the subscription just created "+
			"did not return 200). Liveness polling is DISABLED for this peer "+
			"- polling it would resubscribe on every tick and create a new "+
			"undeletable subscription each time. KNOWN NON-COMPLIANCE: "+
			"TS 29.508 clause 5.2.3 / TS 29.518 clause 5.2.2.4 require the "+
			"Individual Subscription resource.", peer)
}

func livenessPollable(peer string) bool {
	st := stateFor(peer)
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.livenessProbed && st.livenessSupported
}

// ------------------------------------------------------------------------------
// discoverPeerNfInstanceId - one Nnrf_NFDiscovery_Request for the peer's NF
// type (TS 23.288 clause 6.2.2.4, TS 23.502 clause 4.17.4).
//
// Returns "" when the NRF is not configured, cannot be reached, or does not
// know the peer. An empty result must never be read as "the peer restarted".
func discoverPeerNfInstanceId(peer string) string {
	if config.Nrf.Uri == "" {
		return ""
	}
	url := strings.TrimRight(config.Nrf.Uri, "/") +
		"/nnrf-disc/v1/nf-instances?target-nf-type=" + peer +
		"&requester-nf-type=NWDAF"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return ""
	}
	resp, err := subscriptionClient(config.Nrf.HttpVersion).Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return ""
	}
	var result struct {
		NfInstances []struct {
			NfInstanceId string `json:"nfInstanceId"`
		} `json:"nfInstances"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return ""
	}
	if len(result.NfInstances) == 0 {
		return ""
	}
	return result.NfInstances[0].NfInstanceId
}

// discoverPeerNfInstanceIds - ALL NF Instance IDs the NRF currently holds for
// this peer, not just the first.
//
// WHY THIS EXISTS. The OAI NFs generate a fresh nfInstanceId on every start and
// never deregister, so the NRF accumulates REGISTERED profiles for one NF -
// measured live: 18 SMF profiles, all REGISTERED, all on 192.168.70.133. The
// discovery response is not ordered, so reading NfInstances[0] returns a
// different id from one poll to the next WITHOUT the peer having restarted.
//
// That made peerRestarted() fire on a stale profile, and each false positive
// created another undeletable event-exposure subscription (neither the OAI SMF
// nor AMF implements the Individual Subscription resource). Every extra
// subscription means each notification is stored again, which inflates every
// summed rate N-fold - the exact corruption usage_report_dedup.go exists to
// mitigate. Observed as a SECOND subscription appearing ~6 minutes after a
// clean redeploy, with nothing restarted in between.
//
// Same defect class as the UPF accumulation already handled by the engine's
// nrf_upf_identity.go, and handled the same way: treat the peer's identity as
// an ordered SET, not a single value.
func discoverPeerNfInstanceIds(peer string) []string {
	nrfUri := strings.TrimRight(config.Nrf.Uri, "/")
	if nrfUri == "" {
		return nil
	}
	target := "SMF"
	if peer == peerAmf {
		target = "AMF"
	}
	url := fmt.Sprintf(
		"%s/nnrf-disc/v1/nf-instances?target-nf-type=%s&requester-nf-type=NWDAF",
		nrfUri, target)
	resp, err := subscriptionClient(config.Nrf.HttpVersion).Get(url)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil
	}
	var result struct {
		NfInstances []struct {
			NfInstanceId string `json:"nfInstanceId"`
		} `json:"nfInstances"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil
	}
	ids := make([]string, 0, len(result.NfInstances))
	for _, i := range result.NfInstances {
		if i.NfInstanceId != "" {
			ids = append(ids, i.NfInstanceId)
		}
	}
	return ids
}

func recordPeerNfInstanceId(peer string) {
	id := discoverPeerNfInstanceId(peer)
	if id == "" {
		log.Printf(
			"%s subscription: the peer's NF Instance ID could not be read from "+
				"the NRF, so a peer restart cannot be detected. Recovery for "+
				"this peer is MANUAL. (For the AMF in this deployment the "+
				"cause is upstream: it is failing its own NRF registration.)",
			peer)
		return
	}
	st := stateFor(peer)
	st.mu.Lock()
	st.peerNfInstanceId = id
	st.mu.Unlock()
	log.Printf("%s subscription: peer NF Instance ID is %s", peer, id)
}

// peerRestarted - true only on POSITIVE evidence that the peer's NF Instance ID
// changed. Unknown-before, unknown-now and NRF-unreachable all return false:
// resubscribing on a guess is what produced the loop described above.
func peerRestarted(peer string) bool {
	st := stateFor(peer)
	st.mu.RLock()
	known := st.peerNfInstanceId
	st.mu.RUnlock()
	if known == "" {
		return false
	}
	// A restart means the id we subscribed under is GONE from the NRF - not
	// merely that some other profile now sorts first. With stale profiles
	// accumulating (see discoverPeerNfInstanceIds), comparing against a single
	// "current" id produces false restarts and duplicate subscriptions.
	ids := discoverPeerNfInstanceIds(peer)
	if len(ids) == 0 {
		return false
	}
	for _, id := range ids {
		if id == known {
			return false // still registered: the peer did not restart
		}
	}
	log.Printf(
		"%s subscription: the NF Instance ID we subscribed under (%s) is no "+
			"longer registered in the NRF among %d profile(s) - treating the "+
			"peer as restarted",
		peer, known, len(ids))
	return true
}

// ------------------------------------------------------------------------------
// ShutdownSubscriptions - TS 23.288 clause 6.2.2.5 "The NWDAF shall subscribe
// (and unsubscribe) to the Event exposure service". Best effort, bounded.
func ShutdownSubscriptions() {
	managerStopOnce.Do(func() { close(managerStop) })
	for _, peer := range []string{peerAmf, peerSmf} {
		uri := stateFor(peer).get()
		if uri == "" {
			continue
		}
		if deleteSubscriptionResource(peer, uri) {
			clearStoredSubscription(peer)
			stateFor(peer).set("")
		}
	}
}
