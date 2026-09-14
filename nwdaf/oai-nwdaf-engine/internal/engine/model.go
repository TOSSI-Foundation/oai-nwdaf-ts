/*
 * SPDX-License-Identifier: LicenseRef-CSSL-1.0
 */

/*
 * This file contains data structures and global variables.
 */

package engine

import (
	"time"

	"go.mongodb.org/mongo-driver/mongo"
)

// Global variable
var mongoClient *mongo.Client
var config EngineConfig

// ------------------------------------------------------------------------------
// Type of EngineConfig structure
type EngineConfig struct {
	Routes struct {
		NumOfUe           string `envconfig:"ENGINE_NUM_OF_UE_ROUTE"`
		SessSuccRatio     string `envconfig:"ENGINE_SESS_SUCC_RATIO_ROUTE"`
		UeComm            string `envconfig:"ENGINE_UE_COMMUNICATION_ROUTE"`
		UeMob             string `envconfig:"ENGINE_UE_MOBILITY_ROUTE"`
		NfLoad            string `envconfig:"ENGINE_NF_LOAD_ROUTE"`
		QosSustainability string `envconfig:"ENGINE_QOS_SUSTAINABILITY_ROUTE"`
		DnPerformance     string `envconfig:"ENGINE_DN_PERFORMANCE_ROUTE" default:"/dn_performance"`
	}
	Database struct {
		Uri                      string `envconfig:"MONGODB_URI"`
		DbName                   string `envconfig:"MONGODB_DATABASE_NAME"`
		CollectionAmfName        string `envconfig:"MONGODB_COLLECTION_NAME_AMF"`
		CollectionSmfName        string `envconfig:"MONGODB_COLLECTION_NAME_SMF"`
		CollectionUpfMetricsName string `envconfig:"MONGODB_COLLECTION_NAME_UPF_METRICS" default:"upf_metrics"`
	}
	NfLoad struct {
		UpfId               string  `envconfig:"NF_LOAD_UPF_ID" default:"vpp-upf"`
		UpfCpuCores         float64 `envconfig:"NF_LOAD_UPF_CPU_CORES" default:"16"`
		UpfMemCapacityBytes int64   `envconfig:"NF_LOAD_UPF_MEM_CAPACITY_BYTES" default:"24000000000"`
		// FALLBACK ONLY. The NF Instance ID reported in TS 23.288 Table 6.5.3-1
		// is normally resolved from the NRF (clause 6.2.2.4) - see
		// nrf_upf_identity.go. This value is used only when NRF_URI is unset or
		// the NRF cannot be reached, and the engine logs which source it used.
		// It has NO default on purpose: a stale or invented UUID that matches no
		// registered NF is worse than an absent identifier, because a consumer
		// cannot tell the two apart.
		UpfNfInstanceId string `envconfig:"NF_LOAD_UPF_NF_INSTANCE_ID"`
	}
	// DnPerformance - DN_PERFORMANCE (TS 23.288 clause 6.14) tuning.
	DnPerformance struct {
		// WindowSec is the default analytics window, in seconds, applied when
		// the consumer supplies no explicit one - see applyDefaultRecentWindow.
		//
		// It is CONFIGURABLE because it dominates the closed-loop reaction
		// latency: a usage report only influences the analytics output once it
		// is inside this window, and the reported rate is an average over it,
		// so a step change in offered load takes on the order of the window to
		// show up. Shortening it trades statistical support for speed - fewer
		// samples in the window means predictDnPerf() has fewer points to fit
		// and reports a lower Confidence, and below 3 samples it omits the
		// prediction entirely rather than inventing one.
		//
		// The default is 300, which is the value that was hard-coded before
		// this became configurable: an unset variable changes no behaviour.
		// A non-positive value is rejected in favour of the default rather
		// than silently disabling the bound, because an unbounded window is
		// the performance problem applyDefaultRecentWindow exists to prevent.
		WindowSec int64 `envconfig:"ENGINE_DN_PERFORMANCE_WINDOW_SEC" default:"300"`
		// HealthMaxAgeSec bounds how old a per-DNAI interface-health sample may
		// be before it is reported as UNKNOWN_STALE instead of being trusted.
		//
		// This is a FRESHNESS bound and is NOT the same thing as WindowSec above,
		// which is an averaging window. The collector samples every ~5 s, so a
		// health reading from 300 s ago describes a moment long past; six poll
		// intervals tolerates a couple of missed polls without tolerating a dead
		// collector. See loadDnaiPathHealth().
		HealthMaxAgeSec int64 `envconfig:"ENGINE_DN_PERFORMANCE_HEALTH_MAX_AGE_SEC" default:"30"`
	}
	// Nrf - Nnrf_NFDiscovery access for UPF identity resolution
	// (TS 23.288 clause 6.2.2.4). Optional: an unset NRF_URI leaves the engine
	// behaving exactly as before, using NfLoad.UpfNfInstanceId.
	Nrf struct {
		Uri               string `envconfig:"NRF_URI"`
		HttpVersion       string `envconfig:"NRF_HTTP_VERSION" default:"2"`
		UpfFqdn           string `envconfig:"NF_LOAD_UPF_NRF_FQDN"`
		UpfNfInstanceName string `envconfig:"NF_LOAD_UPF_NRF_NF_INSTANCE_NAME"`
		RefreshSec        int    `envconfig:"NF_LOAD_UPF_NRF_REFRESH_SEC" default:"30"`
	}
}

// ------------------------------------------------------------------------------
// Type of network_performance data to request engine
type EngineReqData struct {
	StartTs time.Time `json:"startTs,omitempty"`
	EndTs   time.Time `json:"endTs,omitempty"`
	Tais    []Tai     `json:"tais,omitempty"`
	Dnns    []string  `json:"dnns,omitempty"`
	Snssaia []Snssai  `json:"snssaia,omitempty"`
	Supi    string    `json:"supi,omitempty"`
	// NfInstanceIds carries the TS 23.288 clause 6.5.1 Analytics Filter
	// Information "list of NF Instance IDs" through from the NBI. Empty means
	// no filter. NOTE: the engine's HTTP API is a PROJECT-INTERNAL interface
	// between oai-nwdaf-nbi-* and oai-nwdaf-engine, not a 3GPP SBI; only the
	// semantics it carries are standards-defined.
	NfInstanceIds []string `json:"nfInstanceIds,omitempty"`
	// Dnais, AppIds and UpfId carry the TS 23.288 Table 6.14.1-1 DN Performance
	// Analytics Filter Information through from the NBI: "DNAI",
	// "Application ID" and "Anchor UPF info" respectively. Empty means no
	// filter. Dnns and Snssaia above are reused for the DNN and S-NSSAI filters
	// of the same table.
	Dnais  []string `json:"dnais,omitempty"`
	AppIds []string `json:"appIds,omitempty"`
	UpfId  string   `json:"upfId,omitempty"`
	// OffsetPeriod is TS 29.520 EventReportingRequirement.offsetPeriod, in
	// seconds: "if the value is negative means statistics in the past offset
	// period, otherwise a positive value means prediction in the future offset
	// period". A POSITIVE value therefore asks this engine for TS 23.288
	// Table 6.14.3-2 PREDICTIONS instead of Table 6.14.3-1 statistics.
	// Zero or negative = statistics, which is the default and the pre-existing
	// behaviour.
	OffsetPeriod int32 `json:"offsetPeriod,omitempty"`
}

type Tai struct {
	PlmnId PlmnId `json:"plmnId"`
	Tac    string `json:"tac"`
	Nid    string `json:"nid,omitempty"`
}

type PlmnId struct {
	Mcc string `json:"mcc"`
	Mnc string `json:"mnc"`
}

type Snssai struct {
	Sst int32  `json:"sst"`
	Sd  string `json:"sd,omitempty"`
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
// Type of Nf_load response from engine.
type NfLoadResp struct {
	// NfType and NfInstanceId are TS 23.288 Table 6.5.3-1 outputs ("NF type",
	// "NF instance ID"). NfInstanceId is resolved from the NRF where possible -
	// see nrf_upf_identity.go.
	NfType       string `json:"nfType"`
	NfInstanceId string `json:"nfInstanceId"`
	// No omitempty on the load values: 0 is a valid reading (an idle UPF) and
	// must be distinguishable from "no reading at all". With omitempty an idle
	// UPF serialised to {} and every consumer saw "no data".
	NfCpuUsage         int32 `json:"nfCpuUsage"`
	NfMemoryUsage      int32 `json:"nfMemoryUsage"`
	NfLoadLevelAverage int32 `json:"nfLoadLevelAverage"`
	NfLoadLevelpeak    int32 `json:"nfLoadLevelpeak"`
	// SampleCount is PROJECT-INTERNAL diagnostics, not a TS 23.288 output. It
	// replaces the former "Confidence" field: TS 23.288 Table 6.5.3-1 (NF load
	// STATISTICS, which is what this engine produces) has NO Confidence
	// attribute - only Table 6.5.3-2 (predictions) does - and the value that
	// used to be sent was min(len(samples)*10, 100), a sample count dressed up
	// as a statistical confidence. It is reported here under an honest name and
	// is NOT forwarded to the 3GPP NfLoadLevelInformation model.
	SampleCount int32 `json:"sampleCount"`
	// IdentitySource is PROJECT-INTERNAL: "NRF" (TS 23.288 clause 6.2.2.4) or
	// "configured" (fallback). Lets the NBI and the logs state where the
	// reported NF Instance ID came from instead of implying discovery happened.
	IdentitySource string `json:"identitySource,omitempty"`
}

// ------------------------------------------------------------------------------
// Types of Dn_performance response from engine - TS 23.288 Table 6.14.3-1.
//
// The nesting mirrors the table: one DnPerfResp per (Application ID, S-NSSAI,
// DNN), each carrying a list of per-path "DN performance" records.
//
// There is NO Confidence field anywhere in these types, deliberately. Table
// 6.14.3-1 (STATISTICS, which is what this engine produces) has none; only
// Table 6.14.3-2 (predictions) does.
type DnPerfResp struct {
	// AppId is never populated by this engine - no per-application traffic
	// classification exists. It stays in the model because the 3GPP DnPerfInfo
	// carries it, and omitempty keeps it off the wire.
	AppId  string        `json:"appId,omitempty"`
	Dnn    string        `json:"dnn,omitempty"`
	Snssai *Snssai       `json:"snssai,omitempty"`
	DnPerf []DnPerfEntry `json:"dnPerf"`
	// Confidence is TS 23.288 Table 6.14.3-2 and is set ONLY for predictions.
	// Statistics (Table 6.14.3-1) have no such attribute and must not carry
	// one - omitempty keeps it off the wire for them.
	//
	// WHAT IT MEANS HERE, precisely, because TS 23.288 does not define how a
	// producer computes it: this NWDAF reports how STABLE the observed series
	// was - a function of how many usage reports contributed and how dispersed
	// they were. It is NOT a validated forecast-accuracy probability and is NOT
	// calibrated against outcomes. See predictDnPerf() in dn_performance.go.
	// CLASSIFICATION: the attribute and its placement are 3GPP; the formula is
	// PROJECT-SPECIFIC.
	Confidence int32 `json:"confidence,omitempty"`
}

// DnPerfEntry - one Table 6.14.3-1 "DN performance" record: which path, and how
// it performed.
type DnPerfEntry struct {
	// UpfId is Table 6.14.3-1 "> Serving anchor UPF info", resolved from the
	// NRF (TS 23.288 clause 6.2.2.4) - see nrf_upf_identity.go.
	UpfId string `json:"upfId,omitempty"`
	// Dnai is Table 6.14.3-1 "> DNAI", from TS 29.508 UP_PATH_CH.
	Dnai     string       `json:"dnai,omitempty"`
	PerfData PerfDataResp `json:"perfData"`
	// TemporalValidCon is Table 6.14.3-1 "> Temporal Validity Condition": the
	// period the figures were actually measured over.
	TemporalValidCon *TimeWindowResp `json:"temporalValidCon,omitempty"`
	// SampleCount is PROJECT-INTERNAL diagnostics, NOT a TS 23.288 output and
	// NOT forwarded to the 3GPP DnPerf model. It is the number of distinct PFCP
	// usage reports that contributed, so a thin measurement can be recognised
	// as thin instead of being mistaken for a confident one.
	SampleCount int32 `json:"sampleCount,omitempty"`
	// Predicted marks an entry whose perfData is an EXTRAPOLATION rather than a
	// measurement. PROJECT-INTERNAL diagnostics, not a 3GPP field: on the wire
	// the distinction is carried by the presence of Confidence.
	Predicted bool `json:"predicted,omitempty"`
	// PathHealth is a VENDOR EXTENSION, not part of TS 23.288 Table 6.14.3-1.
	// It is per-DNAI/per-path and comes from UPF interface counters, NOT from
	// the per-SUPI usage-report timeline that fills PerfData - which is exactly
	// why it can exist for a DNAI carrying no PDU session at all. Pointer so an
	// entry with no telemetry omits it rather than publishing a zeroed struct.
	PathHealth *PathHealthResp `json:"oaiPathHealthExt,omitempty"`
}

// PathHealthResp - per-DNAI path health derived from UPF N6 interface counters.
//
// THIS IS NOT A 3GPP METRIC AND MUST NOT BE PRESENTED AS ONE.
// The JSON key is vendor-prefixed (`oaiPathHealthExt`) precisely so that no
// consumer can mistake it for Table 6.14.3-1 output. In particular it is NOT
// avgPacketLossRate: across every impairment measured, VPP interface `drops`,
// Linux `tx_dropped` and `tx_errors` all stayed at exactly zero, so nothing
// here counts a discarded packet.
//
// What it measures is the fraction of transmit attempts on this DNAI's N6
// interface that VPP could not hand to the kernel socket - AF_PACKET
// transmit-side backpressure, specific to this VPP-on-veth deployment.
type PathHealthResp struct {
	// State is one of OBSERVED_HEALTHY / OBSERVED_DEGRADED /
	// UNKNOWN_NO_TRAFFIC / UNKNOWN_INSUFFICIENT_SAMPLES / UNKNOWN_NO_SAMPLE /
	// UNKNOWN_STALE, copied verbatim from the collector - this layer does not
	// whitelist it, so a producer-side state change needs no change here.
	//
	// State is the whole point of this struct. UNKNOWN_* is NOT "healthy":
	// an idle path makes no transmit attempt, so it CANNOT produce a failure,
	// and an idle impaired path is byte-for-byte identical to an idle healthy
	// one (measured). A consumer that reads absence of
	// failures as health will steer into a path it knows nothing about.
	State string `json:"state"`
	// SendtoFailurePerPacket is a POINTER so that UNKNOWN serialises as an
	// explicit JSON null. It must never be flattened to 0.
	SendtoFailurePerPacket *float64 `json:"sendtoFailurePerPacket"`
	// Numerator and denominator are published alongside the ratio so it can be
	// recomputed, re-normalised or replaced without re-running any experiment.
	TxAttempts     int64  `json:"txAttempts"`
	SendtoFailures int64  `json:"sendtoFailures"`
	Denominator    string `json:"denominator,omitempty"`
	Semantics      string `json:"semantics,omitempty"`
	// ObservedAt / AgeSec make staleness auditable by the consumer rather than
	// implicit in the producer.
	ObservedAt int64 `json:"observedAt,omitempty"`
	AgeSec     int64 `json:"ageSec"`
}

// PerfDataResp - Table 6.14.3-1 "> Performance Data".
//
// ONLY the two traffic-rate fields exist here. Average/Maximum Packet Delay and
// Average Packet Loss Rate are NOT modelled at all: the SMF returns null for
// ulDelays/dlDelays/rtDelays and PFCP usage reports carry no loss counter.
// Table 6.4.2-2 NOTE 1 makes that a Rel-17 gap in the specification itself,
// not an OAI defect. Absent, never zeroed.
//
// The rates are TS 29.571 BitRate strings (e.g. "445.5 Mbps"), which is what
// the 3GPP PerfData model expects - not numbers.
type PerfDataResp struct {
	AvgTrafficRate string `json:"avgTrafficRate,omitempty"`
	MaxTrafficRate string `json:"maxTrafficRate,omitempty"`
}

// TimeWindowResp - TS 29.571 TimeWindow. Both fields are required by the model.
type TimeWindowResp struct {
	StartTime time.Time `json:"startTime"`
	StopTime  time.Time `json:"stopTime"`
}

// ------------------------------------------------------------------------------
// Type of Qos_sustainability response from engine.
type QosSustainabilityResp struct {
	RanUeThrouThd string `json:"ranUeThrouThd,omitempty"`
	Confidence    int32  `json:"confidence,omitempty"`
}
