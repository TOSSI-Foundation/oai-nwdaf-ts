/*
 * SPDX-License-Identifier: LicenseRef-CSSL-1.0
 */

/*
 * This file contains data structures and global variables.
 */

package sbi

import (
	amf_client "gitlab.eurecom.fr/oai/cn5g/oai-cn5g-nwdaf/components/oai-nwdaf-sbi/internal/amfclient"
	smf_client "gitlab.eurecom.fr/oai/cn5g/oai-cn5g-nwdaf/components/oai-nwdaf-sbi/internal/smfclient"
	"go.mongodb.org/mongo-driver/mongo"
)

// Global variable
var mongoClient *mongo.Client
var config SbiConfig

// ------------------------------------------------------------------------------
// Type of EngineConfig structure
type SbiConfig struct {
	Amf struct {
		IpAddr            string `envconfig:"AMF_IP_ADDR"`
		SubRoute          string `envconfig:"AMF_SUBSCR_ROUTE"`
		ApiRoute          string `envconfig:"AMF_API_ROUTE"`
		NotifCorrId       string `envconfig:"AMF_NOTIFY_CORRELATION_ID"`
		NotifId           string `envconfig:"AMF_NOTIFICATION_ID"`
		NorifForwardRoute string `envconfig:"AMF_NOTIFICATION_FORWARD_ROUTE"`
		// Was accepted as an env var but never read by any code path; see
		// subscriptionClient() in utils.go.
		HttpVersion string `envconfig:"AMF_HTTP_VERSION"`
	}
	Smf struct {
		IpAddr            string `envconfig:"SMF_IP_ADDR"`
		SubRoute          string `envconfig:"SMF_SUBSCR_ROUTE"`
		ApiRoute          string `envconfig:"SMF_API_ROUTE"`
		NotifCorrId       string `envconfig:"SMF_NOTIFY_CORRELATION_ID"`
		NotifId           string `envconfig:"SMF_NOTIFICATION_ID"`
		NorifForwardRoute string `envconfig:"SMF_NOTIFICATION_FORWARD_ROUTE"`
		HttpVersion       string `envconfig:"SMF_HTTP_VERSION"`
	}
	Database struct {
		Uri               string `envconfig:"MONGODB_URI"`
		DbName            string `envconfig:"MONGODB_DATABASE_NAME"`
		CollectionAmfName string `envconfig:"MONGODB_COLLECTION_NAME_AMF"`
		CollectionSmfName string `envconfig:"MONGODB_COLLECTION_NAME_SMF"`
		// QosMonRetain - how many of the most recent PFCP usage reports to keep
		// in each SUPI's qosmonlist. 0 or negative disables the bound and
		// restores the previous unbounded append.
		//
		// WHY THIS EXISTS. qosmonlist grows by one entry per usage report per
		// PDU session - about one every 5 s here - and nothing ever removed
		// them. Measured live after 39 h: 17 954 entries in a single ~10 MB
		// document, of which the 300 s analytics window used 60. The engine
		// selects documents with an $elemMatch on the timestamp but then walks
		// the WHOLE array in application code (see applyDefaultRecentWindow in
		// the engine's utils.go, which already records 6.5 s spent scanning
		// 248k entries), so the cost is driven by total array length rather
		// than by how much of it is in the window. The result was
		// nnwdaf-analyticsinfo latency of 3-4.5 s and SMF requests timing out
		// with "NWDAF returned HTTP 0".
		//
		// SIZING. At the measured ~573 bytes per entry and ~1 entry / 5 s:
		//   default window 300 s  ->    60 entries
		//   1000 entries          ->  ~83 min of history, ~570 KB per document
		// 1000 therefore leaves roughly 16x headroom over the default window,
		// so a consumer asking for an explicit window well beyond 300 s still
		// finds its data, while bounding a document that had reached 10 MB.
		// Raise it if you routinely query windows longer than an hour.
		QosMonRetain int `envconfig:"MONGODB_QOSMON_RETAIN" default:"1000"`
	}
	Server struct {
		NotifUri string `envconfig:"EVENT_NOTIFY_URI"`
		Uri      string `envconfig:"SERVER_ADDR"`
	}
	// Nrf - OPTIONAL. Used only to notice that an AMF/SMF restarted, by
	// watching its NF Instance ID (TS 23.288 clause 6.2.2.4: NF/NF service
	// discovery "may be performed on a periodic basis"). The event-exposure
	// endpoints themselves stay configured; this does not replace them.
	// With NRF_URI unset the collector behaves exactly as before and a peer
	// restart is not detected.
	Nrf struct {
		Uri         string `envconfig:"NRF_URI"`
		HttpVersion string `envconfig:"NRF_HTTP_VERSION" default:"2"`
	}
}
type pduSesEst struct {
	AdIpv4Addr  *string
	Dnn         *string
	PduSeId     *int32
	PduSessType *smf_client.PduSessionType
	Snssai      *smf_client.Snssai
	TimeStamp   int64
}

type ueIpCh struct {
	AdIpv4Addr *string
	PduSeId    *int32
	TimeStamp  int64
}

// upPathCh - one TS 29.508 UP_PATH_CH notification.
//
// WHY IT EXISTS: TS 23.288 clause 6.14.2 routes DN Performance analytics'
// SMF-side input through Table 6.4.2-2, which lists DNAI and UPF info as
// SMF-sourced. This is the ONLY record of which DNAI a PDU session's traffic
// is flowing over, so without it a usage report cannot be attributed to a
// DNAI and DN_PERFORMANCE - whose whole output is per-DNAI (Table 6.14.3-1) -
// is not computable.
//
// SourceDnai is a POINTER and is nil for the first notification of a session:
// TS 29.508 makes sourceDnai conditional and there is no previous DNAI at
// establishment. Absent, never faked.
//
// DnaiChgType and UpfInfo are NOT stored. The OAI SMF does not produce them:
// it has no early-notification path (TS 23.502 clause 4.3.6.3) and it discards
// the UPF's nfInstanceId when it processes the UPF profile, so it cannot
// supply "Serving anchor UPF info". The engine resolves the UPF identity from
// the NRF instead (nrf_upf_identity.go, TS 23.288 clause 6.2.2.4).
//
// TimeStamp is the SBI's receive time in Unix seconds, the same convention
// qosMon uses, so a usage report and a DNAI interval are directly comparable
// without converting between clocks.
type upPathCh struct {
	SourceDnai *string
	TargetDnai *string
	PduSeId    *int32
	TimeStamp  int64
}

type ddds struct {
	DddStatus *smf_client.DlDataDeliveryStatus
	PduSeId   *int32
	TimeStamp int64
}

type qosMon struct {
	Customized_data *smf_client.CustomizedData
	PduSeId         *int32
	TimeStamp       int64
	// QoS Monitoring delay measurements per TS 29.508 (uplink / downlink /
	// round-trip, in ms). These are arrays - a single report can carry several
	// samples - so jitter is derivable as the variance within one report.
	// Stored as-is; they stay nil/absent if the SMF doesn't populate them.
	UlDelays []int32
	DlDelays []int32
	RtDelays []int32
}

type rmInfo struct {
	RmInfo    amf_client.RmInfo
	TimeStamp int64
}

type location struct {
	UserLocation amf_client.UserLocation
	TimeStamp    int64
}

type lossOfConnectReason struct {
	LossOfConnectReason amf_client.LossOfConnectivityReasonAnyOf
	TimeStamp           int64
}
