/*
 * SPDX-License-Identifier: LicenseRef-CSSL-1.0
 */

/*
 * NRF registration for this NWDAF instance.
 *
 * WHY THIS EXISTS
 * 3GPP TS 23.288 V17.12.0 clause 5.1 requires that "each NWDAF instance should
 * provide the list of supported Analytics ID(s) (possibly per supported
 * service) when registering to the NRF, in addition to other NRF registration
 * elements of the NF profile", and clause 5.2 (NWDAF Discovery and Selection)
 * makes the NRF the mechanism by which a consumer NF finds an NWDAF that
 * supports the analytics it needs.
 *
 * Before this file, nothing in this repository ever contacted an NRF: an SMF
 * performing a target-nf-type=NWDAF discovery got an empty list, so no
 * spec-compliant consumer could locate this NWDAF at all.
 *
 * ONE NF INSTANCE, TWO SERVICES
 * TS 23.288 clause 7.1 Table 7.1-1 lists Nnwdaf_AnalyticsInfo and
 * Nnwdaf_AnalyticsSubscription (named Nnwdaf_EventsSubscription in the TS
 * 29.520 stage-3 API) as two services OF THE SAME NF. They run here as two
 * containers on two addresses, so both are advertised as separate nfServices
 * of a single NWDAF NF profile, each with its own ipEndPoints. That is what a
 * consumer expects and is why registration lives in one component only.
 *
 * KNOWN OAI NRF LIMITATION (documented, not worked around)
 * The nwdafInfo object below is spec-correct on the wire, but the OAI NRF has
 * no NwdafInfo model: api_conv::profile_api_to_nrf_profile() stores no
 * nwdafInfo, so eventIds/nwdafEvents are accepted and silently dropped, and
 * per-Analytics-ID discovery filtering does not work against this NRF.
 * Discovery by NF type and by service name does work.
 */

package analytics

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"time"

	"golang.org/x/net/http2"
)

// ------------------------------------------------------------------------------
// NrfConfig - NRF registration parameters. Registration is OPT-IN: if NRF_URI
// is empty the NWDAF behaves exactly as before and never contacts an NRF.
type NrfConfig struct {
	Uri           string `envconfig:"NRF_URI"`
	HttpVersion   string `envconfig:"NRF_HTTP_VERSION" default:"2"`
	NfInstanceId  string `envconfig:"NWDAF_NF_INSTANCE_ID"`
	AnalyticsAddr string `envconfig:"NWDAF_ANALYTICS_IPV4"`
	AnalyticsPort int    `envconfig:"NWDAF_ANALYTICS_PORT" default:"8080"`
	EventsAddr    string `envconfig:"NWDAF_EVENTS_IPV4"`
	EventsPort    int    `envconfig:"NWDAF_EVENTS_PORT" default:"8080"`
	HeartbeatSec  int    `envconfig:"NWDAF_HEARTBEAT_SEC" default:"10"`
}

// ------------------------------------------------------------------------------
// Minimal TS 29.510 NFProfile subset. Only the attributes the OAI NRF actually
// reads are populated - inventing profile fields would not make discovery work
// and would misrepresent what this NWDAF supports.
type nfService struct {
	ServiceInstanceId string       `json:"serviceInstanceId"`
	ServiceName       string       `json:"serviceName"`
	Versions          []nfVersion  `json:"versions"`
	Scheme            string       `json:"scheme"`
	NfServiceStatus   string       `json:"nfServiceStatus"`
	IpEndPoints       []ipEndPoint `json:"ipEndPoints"`
}

type nfVersion struct {
	ApiVersionInUri string `json:"apiVersionInUri"`
	ApiFullVersion  string `json:"apiFullVersion"`
}

type ipEndPoint struct {
	Ipv4Address string `json:"ipv4Address"`
	Transport   string `json:"transport"`
	Port        int    `json:"port"`
}

// nwdafInfo - TS 29.510 NwdafInfo. eventIds are the Analytics IDs offered over
// Nnwdaf_AnalyticsInfo; nwdafEvents those offered over Nnwdaf_EventsSubscription.
type nwdafInfo struct {
	EventIds    []string `json:"eventIds,omitempty"`
	NwdafEvents []string `json:"nwdafEvents,omitempty"`
}

type nfProfile struct {
	NfInstanceId   string      `json:"nfInstanceId"`
	NfInstanceName string      `json:"nfInstanceName,omitempty"`
	NfType         string      `json:"nfType"`
	NfStatus       string      `json:"nfStatus"`
	HeartBeatTimer int         `json:"heartBeatTimer,omitempty"`
	Ipv4Addresses  []string    `json:"ipv4Addresses,omitempty"`
	Priority       int         `json:"priority,omitempty"`
	Capacity       int         `json:"capacity,omitempty"`
	NfServices     []nfService `json:"nfServices,omitempty"`
	NwdafInfo      *nwdafInfo  `json:"nwdafInfo,omitempty"`
}

var nrfConfig NrfConfig

// ------------------------------------------------------------------------------
// nrfClient - the OAI NRF's SBI server is nghttp2 in cleartext HTTP/2 and never
// answers an HTTP/1.1 request (the call hangs rather than failing), which is the
// same trap documented in oai-nwdaf-sbi/internal/sbi/utils.go. Default to h2c.
func nrfClient() *http.Client {
	if nrfConfig.HttpVersion != "2" {
		return &http.Client{Timeout: 10 * time.Second}
	}
	return &http.Client{
		Timeout: 10 * time.Second,
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
// buildNfProfile - assemble the NWDAF NF profile.
//
// The Analytics ID lists are taken from what the two NBI services actually
// dispatch (their switch statements), NOT from the full 3GPP enum: advertising
// an Analytics ID we cannot serve would make discovery actively harmful.
// TRAFFIC_STEERING_UPF_LOAD is deliberately NOT advertised - it is a custom,
// non-3GPP Analytics ID (see model_event_id_any_of.go) and a consumer
// discovering this NWDAF must not be told it is a standard analytic.
func buildNfProfile() nfProfile {
	// Only Analytics IDs this NWDAF actually SERVES are advertised. Advertising
	// one it cannot serve is worse than not advertising it: a consumer that
	// discovered it properly would then ask and get a 400.
	//
	// TRAFFIC_STEERING_UPF_LOAD is deliberately ABSENT from both lists and must
	// stay absent. It is NOT a TS 23.288 Table 7.1-2 Analytics ID - it is this
	// project's custom research extension - and an NF must never be led to
	// treat a vendor extension as a standard analytic.
	analyticsInfoEventIds := []string{
		"NETWORK_PERFORMANCE",
		"UE_COMMUNICATION",
		"UE_MOBILITY",
		"NF_LOAD",
		"QOS_SUSTAINABILITY",
		// TS 23.288 clause 6.14 DN Performance Analytics.
		"DN_PERFORMANCE",
	}
	eventsSubEventIds := []string{
		"NETWORK_PERFORMANCE",
		"UE_COMMUNICATION",
		"UE_MOBILITY",
		"NF_LOAD",
		"QOS_SUSTAINABILITY",
		"ABNORMAL_BEHAVIOUR",
		"DN_PERFORMANCE",
	}

	services := []nfService{
		{
			ServiceInstanceId: "nnwdaf-analyticsinfo-1",
			ServiceName:       "nnwdaf-analyticsinfo",
			Versions: []nfVersion{
				{ApiVersionInUri: "v1", ApiFullVersion: "1.2.2"},
			},
			Scheme:          "http",
			NfServiceStatus: "REGISTERED",
			IpEndPoints: []ipEndPoint{{
				Ipv4Address: nrfConfig.AnalyticsAddr,
				Transport:   "TCP",
				Port:        nrfConfig.AnalyticsPort,
			}},
		},
	}
	// Only advertise the subscription service if its address is configured -
	// pointing a consumer at an endpoint that is not deployed is worse than
	// not advertising it.
	if nrfConfig.EventsAddr != "" {
		services = append(services, nfService{
			ServiceInstanceId: "nnwdaf-eventssubscription-1",
			ServiceName:       "nnwdaf-eventssubscription",
			Versions: []nfVersion{
				{ApiVersionInUri: "v1", ApiFullVersion: "1.2.3"},
			},
			Scheme:          "http",
			NfServiceStatus: "REGISTERED",
			IpEndPoints: []ipEndPoint{{
				Ipv4Address: nrfConfig.EventsAddr,
				Transport:   "TCP",
				Port:        nrfConfig.EventsPort,
			}},
		})
	}

	return nfProfile{
		NfInstanceId:   nrfConfig.NfInstanceId,
		NfInstanceName: "OAI-NWDAF",
		NfType:         "NWDAF",
		NfStatus:       "REGISTERED",
		HeartBeatTimer: nrfConfig.HeartbeatSec,
		Ipv4Addresses:  []string{nrfConfig.AnalyticsAddr},
		Priority:       1,
		Capacity:       100,
		NfServices:     services,
		NwdafInfo: &nwdafInfo{
			EventIds:    analyticsInfoEventIds,
			NwdafEvents: eventsSubEventIds,
		},
	}
}

// ------------------------------------------------------------------------------
// registerOnce - PUT the NF profile (TS 29.510 NFRegister).
func registerOnce(client *http.Client) error {
	profile := buildNfProfile()
	body, err := json.Marshal(profile)
	if err != nil {
		return err
	}
	url := fmt.Sprintf(
		"%s/nnrf-nfm/v1/nf-instances/%s", nrfConfig.Uri, nrfConfig.NfInstanceId)
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := ioutil.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK &&
		resp.StatusCode != http.StatusCreated {
		return fmt.Errorf(
			"NRF registration returned HTTP %d: %s",
			resp.StatusCode, string(respBody))
	}
	log.Printf(
		"Registered NWDAF with NRF (instance %s, HTTP %d)",
		nrfConfig.NfInstanceId, resp.StatusCode)
	return nil
}

// ------------------------------------------------------------------------------
// InitNrfRegistration - register with the NRF and keep the registration alive.
//
// The refresh is a full idempotent re-PUT rather than the TS 29.510 PATCH
// heartbeat. The OAI NRF applies its OWN configured heartbeat timer to the
// profile (it overwrites whatever the NF asks for in
// nrf_app::handle_register_nf_instance) and marks an NF suspended when it
// lapses, so what matters here is refreshing inside that interval; re-PUT is
// the operation this NRF is known to accept and is safe to repeat.
//
// Registration failures are logged and retried, never fatal: the NWDAF must
// keep serving its existing consumers (including the traffic-steering
// controller) if the NRF is down.
func InitNrfRegistration() {
	if nrfConfig.Uri == "" {
		log.Printf("NRF_URI not set, skipping NRF registration")
		return
	}
	if nrfConfig.NfInstanceId == "" {
		log.Printf(
			"NWDAF_NF_INSTANCE_ID not set, skipping NRF registration " +
				"(the NRF requires a valid version-4 UUID)")
		return
	}
	if nrfConfig.AnalyticsAddr == "" {
		log.Printf(
			"NWDAF_ANALYTICS_IPV4 not set, skipping NRF registration " +
				"(a consumer could not reach the advertised service)")
		return
	}
	client := nrfClient()
	go func() {
		interval := time.Duration(nrfConfig.HeartbeatSec) * time.Second
		if interval <= 0 {
			interval = 10 * time.Second
		}
		for {
			if err := registerOnce(client); err != nil {
				log.Printf("NRF registration failed: %v", err)
			}
			time.Sleep(interval)
		}
	}()
}

// ------------------------------------------------------------------------------
// DeregisterFromNrf - TS 29.510 NFDeregister. Best effort, called on shutdown.
func DeregisterFromNrf() {
	if nrfConfig.Uri == "" || nrfConfig.NfInstanceId == "" {
		return
	}
	url := fmt.Sprintf(
		"%s/nnrf-nfm/v1/nf-instances/%s", nrfConfig.Uri, nrfConfig.NfInstanceId)
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		return
	}
	resp, err := nrfClient().Do(req)
	if err != nil {
		log.Printf("NRF deregistration failed: %v", err)
		return
	}
	defer resp.Body.Close()
	log.Printf("Deregistered NWDAF from NRF (HTTP %d)", resp.StatusCode)
}
