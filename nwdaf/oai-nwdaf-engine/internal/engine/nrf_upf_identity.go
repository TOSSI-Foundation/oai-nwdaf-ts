/*
 * SPDX-License-Identifier: LicenseRef-CSSL-1.0
 */

/*
 * NF Instance ID identity for the UPF whose load this NWDAF reports.
 *
 * WHY THIS EXISTS
 * -----------------------------------------------------------------------------
 * 3GPP-STANDARD BEHAVIOUR being implemented here:
 *
 *   TS 23.288 V17.12.0 clause 6.5.3, Table 6.5.3-1 ("NF load statistics") lists
 *   "NF instance ID - Identification of the NF instance" as an output of NF load
 *   analytics, and clause 6.5.1 lets a consumer scope the request with "an
 *   optional list of NF Instance IDs, NF Set IDs, or NF types". Both are
 *   meaningless unless the identifier the NWDAF reports is the identifier the
 *   rest of the network actually uses for that NF.
 *
 *   TS 23.288 clause 6.2.2.4 ("Procedure for Data Collection from NRF") is the
 *   standard-defined way to obtain it: the NWDAF may use "NF/NF service
 *   discovery procedures ... and Nnrf_NFDiscovery service ... in order to
 *   dynamically discover the NF instances and services of the 5GC. Such
 *   discovery may be performed on a periodic basis", and the clause states
 *   explicitly that the returned NF Profiles "can be used to set-up and maintain
 *   a consistent network map for data collection and also, depending on use
 *   cases, to perform analytics (e.g. NF load analytics as defined in clause
 *   6.5)".
 *
 * WHAT WAS WRONG BEFORE (OAI/project implementation gap, now fixed)
 * -----------------------------------------------------------------------------
 * NF_LOAD_UPF_NF_INSTANCE_ID was a STATIC configured UUID. The deployed value,
 * "4f4f850a-552e-4a60-9d31-a108271e3daf", was NOT invented - it was the UPF's
 * REAL NF Instance ID at an earlier boot (it is the first entry in the vpp-upf
 * container's own NRF-registration log). It went STALE the moment the UPF
 * restarted and generated a new one, and by 2026-08-25 the NRF no longer held
 * it at all.
 *
 * The SMF was configured with the same stale constant, so the clause-6.5.1
 * filter check compared one dangling identifier against another and agreed for
 * the wrong reason - it would have broken the instant either side was corrected
 * on its own. A pinned identifier cannot survive a peer that re-identifies
 * itself on every restart; only discovery can.
 *
 * KNOWN UPSTREAM DEFECT THIS MUST TOLERATE (OAI upstream defect, not fixed here)
 * -----------------------------------------------------------------------------
 * The OAI VPP-UPF generates a NEW nfInstanceId on every start and never
 * deregisters the previous one (observed in the vpp-upf container log:
 * 106458a0-... -> a1406da5-... -> b8c83e73-...), so the NRF accumulates several
 * REGISTERED profiles that all describe the SAME physical UPF. TS 29.510 clause
 * 5.2.2.2 defines NFDeregister for exactly this, and the UPF does not call it.
 *
 * Consequently this resolver keeps the ORDERED SET of NF Instance IDs the NRF
 * currently holds for the measured UPF, not a single value:
 *
 *   - the FIRST one is reported as this analytic's nfInstanceId, because the
 *     OAI SMF's process_upf_profile() also takes the first match by FQDN when it
 *     sets up the PFCP association, so "first" names the UPF the SMF is
 *     really using;
 *   - a clause-6.5.1 filter matches if it names ANY id in the set, because they
 *     all denote the one NF instance this NWDAF has data for. Refusing a
 *     consumer that happened to learn a different duplicate from the NRF would
 *     be wrong: it did designate this NF instance.
 *
 * FALLBACK
 * -----------------------------------------------------------------------------
 * If NRF_URI is unset or the NRF is unreachable, the configured
 * NF_LOAD_UPF_NF_INSTANCE_ID is used and the fact is logged. That keeps the
 * engine working in an NRF-less bring-up, but it is NOT clause 6.2.2.4
 * behaviour and is reported as "configured" rather than "NRF" by
 * upfIdentitySource().
 */

package engine

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http2"
)

// ------------------------------------------------------------------------------
// Minimal subset of the TS 29.510 NFProfile needed to identify a UPF. Only the
// attributes actually used for matching are declared - decoding more of the
// profile than we use would suggest this component understands more of it
// than it does.
type nrfNfProfile struct {
	NfInstanceId   string `json:"nfInstanceId"`
	NfInstanceName string `json:"nfInstanceName"`
	NfType         string `json:"nfType"`
	NfStatus       string `json:"nfStatus"`
	Fqdn           string `json:"fqdn"`
}

type nrfSearchResult struct {
	NfInstances []nrfNfProfile `json:"nfInstances"`
}

// ------------------------------------------------------------------------------
// upfIdentity - cached result of the last successful Nnrf_NFDiscovery_Request.
type upfIdentityCache struct {
	mu        sync.RWMutex
	ids       []string
	fetchedAt time.Time
	fromNrf   bool
}

var upfIdentity upfIdentityCache

// ------------------------------------------------------------------------------
// nrfClient - the OAI NRF's SBI server is nghttp2 in cleartext HTTP/2 and never
// answers an HTTP/1.1 request (it hangs rather than failing), the same trap
// documented in oai-nwdaf-sbi/internal/sbi/utils.go and in the NBI's
// nrf_registration.go. Default to h2c.
func nrfClient() *http.Client {
	if config.Nrf.HttpVersion != "2" {
		return &http.Client{Timeout: 8 * time.Second}
	}
	return &http.Client{
		Timeout: 8 * time.Second,
		Transport: &http2.Transport{
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
// profileMatchesTargetUpf - decide whether an NRF UPF profile describes the UPF
// this engine is measuring.
//
// The engine measures a container (NF_LOAD_UPF_ID, e.g. "vpp-upf") through
// cgroup accounting; the NRF knows that UPF by FQDN and nfInstanceName. Neither
// side shares an identifier, so the mapping is configuration, and it is made
// explicit rather than guessed:
//
//	NF_LOAD_UPF_NRF_FQDN            - substring match on the profile fqdn
//	NF_LOAD_UPF_NRF_NF_INSTANCE_NAME - exact (case-insensitive) nfInstanceName
//
// If neither is set the match falls back to NF_LOAD_UPF_ID as an fqdn
// substring, which is true for the default deployment
// ("vpp-upf" is a prefix of "vpp-upf.node.5gcn.mnc95.mcc208.3gppnetwork.org").
func profileMatchesTargetUpf(p nrfNfProfile) bool {
	if !strings.EqualFold(p.NfType, "UPF") {
		return false
	}
	if fqdn := config.Nrf.UpfFqdn; fqdn != "" {
		return strings.Contains(strings.ToLower(p.Fqdn), strings.ToLower(fqdn))
	}
	if name := config.Nrf.UpfNfInstanceName; name != "" {
		return strings.EqualFold(p.NfInstanceName, name)
	}
	return strings.Contains(
		strings.ToLower(p.Fqdn), strings.ToLower(config.NfLoad.UpfId))
}

// ------------------------------------------------------------------------------
// discoverUpfInstanceIds - one Nnrf_NFDiscovery_Request (TS 29.510 clause
// 5.3.2.2.2, GET /nnrf-disc/v1/nf-instances), filtered to UPFs.
//
// requester-nf-type=NWDAF: TS 29.510 Table 6.1.6.3.3-1 includes NWDAF in the
// NFType enum, and TS 23.288 clause 6.2.2.4 is what authorises an NWDAF to make
// this query at all.
func discoverUpfInstanceIds() ([]string, error) {
	url := strings.TrimRight(config.Nrf.Uri, "/") +
		"/nnrf-disc/v1/nf-instances?target-nf-type=UPF&requester-nf-type=NWDAF"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := nrfClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &nrfError{status: resp.StatusCode, body: string(body)}
	}
	var result nrfSearchResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(result.NfInstances))
	for _, p := range result.NfInstances {
		if !profileMatchesTargetUpf(p) {
			continue
		}
		if p.NfInstanceId == "" {
			continue
		}
		ids = append(ids, p.NfInstanceId)
	}
	return ids, nil
}

type nrfError struct {
	status int
	body   string
}

func (e *nrfError) Error() string {
	return "NRF discovery returned HTTP " + http.StatusText(e.status) + ": " + e.body
}

// ------------------------------------------------------------------------------
// refreshUpfIdentity - refresh the cache if it is older than the configured
// interval. Cheap and safe to call on every request: the NRF is only contacted
// once per NF_LOAD_UPF_NRF_REFRESH_SEC.
func refreshUpfIdentity() {
	if config.Nrf.Uri == "" {
		return
	}
	interval := time.Duration(config.Nrf.RefreshSec) * time.Second
	if interval <= 0 {
		interval = 30 * time.Second
	}
	upfIdentity.mu.RLock()
	fresh := time.Since(upfIdentity.fetchedAt) < interval
	upfIdentity.mu.RUnlock()
	if fresh {
		return
	}
	ids, err := discoverUpfInstanceIds()
	if err != nil {
		log.Printf(
			"NF_LOAD: Nnrf_NFDiscovery for the target UPF failed (%v); "+
				"keeping the previous identity", err)
		// Do not clear the cache: a transient NRF outage must not make the
		// analytic start reporting a different NF Instance ID.
		upfIdentity.mu.Lock()
		upfIdentity.fetchedAt = time.Now()
		upfIdentity.mu.Unlock()
		return
	}
	upfIdentity.mu.Lock()
	defer upfIdentity.mu.Unlock()
	upfIdentity.fetchedAt = time.Now()
	if len(ids) == 0 {
		log.Printf(
			"NF_LOAD: the NRF returned no UPF profile matching the measured "+
				"UPF %q - falling back to the configured NF Instance ID",
			config.NfLoad.UpfId)
		upfIdentity.ids = nil
		upfIdentity.fromNrf = false
		return
	}
	if !sameStrings(upfIdentity.ids, ids) {
		log.Printf(
			"NF_LOAD: UPF NF Instance ID(s) from the NRF: %v (reporting %q)",
			ids, ids[0])
		if len(ids) > 1 {
			log.Printf(
				"NF_LOAD: %d NRF profiles describe the same UPF. This is the "+
					"known OAI UPF defect - it generates a new nfInstanceId on "+
					"every start and never calls NFDeregister (TS 29.510 "+
					"clause 5.2.2.2), so stale registrations accumulate. A "+
					"clause 6.5.1 filter naming ANY of them is accepted.",
				len(ids))
		}
	}
	upfIdentity.ids = ids
	upfIdentity.fromNrf = true
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ------------------------------------------------------------------------------
// upfInstanceIds - the ordered set of NF Instance IDs that denote the measured
// UPF, and whether they came from the NRF.
func upfInstanceIds() ([]string, bool) {
	upfIdentity.mu.RLock()
	defer upfIdentity.mu.RUnlock()
	if upfIdentity.fromNrf && len(upfIdentity.ids) > 0 {
		out := make([]string, len(upfIdentity.ids))
		copy(out, upfIdentity.ids)
		return out, true
	}
	if config.NfLoad.UpfNfInstanceId == "" {
		return nil, false
	}
	return []string{config.NfLoad.UpfNfInstanceId}, false
}

// ------------------------------------------------------------------------------
// primaryUpfInstanceId - the NF Instance ID reported in Table 6.5.3-1's
// "NF instance ID" output field. Empty means "this NWDAF cannot identify the NF
// it is measuring", which callers must surface rather than paper over.
func primaryUpfInstanceId() string {
	ids, _ := upfInstanceIds()
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

// ------------------------------------------------------------------------------
// upfIdentitySource - "NRF" (clause 6.2.2.4) or "configured" (fallback).
func upfIdentitySource() string {
	_, fromNrf := upfInstanceIds()
	if fromNrf {
		return "NRF"
	}
	return "configured"
}

// ------------------------------------------------------------------------------
// nfInstanceFilterMatches - TS 23.288 clause 6.5.1:
//
//	"If a list of the NF Instance IDs (or respectively of NF Set IDs) is
//	 provided, the NWDAF shall provide the analytics for each designated NF
//	 instance"
//
// An empty list is "no filter" and matches. A non-empty list matches only if it
// designates the NF instance this NWDAF actually has data for. Anything else
// must produce NO analytics for that NF - never this NF's analytics under
// another NF's identity.
func nfInstanceFilterMatches(requested []string) bool {
	if len(requested) == 0 {
		return true
	}
	ids, _ := upfInstanceIds()
	for _, want := range requested {
		for _, have := range ids {
			if strings.EqualFold(strings.TrimSpace(want), have) {
				return true
			}
		}
	}
	return false
}
