/*
 * SPDX-License-Identifier: LicenseRef-CSSL-1.0
 */

/*
 * Custom (non-3GPP) model for the TRAFFIC_STEERING_UPF_LOAD analytic ID:
 * AI-driven congestion forecast / recommended-UPF prediction, produced by
 * oai-nwdaf-engine-traffic-steering. Not part of the generated TS29520
 * OpenAPI models since 3GPP defines no standard analytics ID for this
 * composite use case (it consumes NF_LOAD + QoS telemetry as inputs).
 */

package events

// TrafficSteeringInfo - Represents an AI-driven congestion forecast for a UPF.
type TrafficSteeringInfo struct {
	UpfId string `json:"upfId"`

	// Predicted probability (0-1) of SLA violation within HorizonSec.
	CongestionScore float32 `json:"congestionScore"`

	PredictedSlaViolation bool `json:"predictedSlaViolation"`

	// Prediction horizon in seconds.
	HorizonSec int32 `json:"horizonSec,omitempty"`

	ModelVersion string `json:"modelVersion,omitempty"`
}
