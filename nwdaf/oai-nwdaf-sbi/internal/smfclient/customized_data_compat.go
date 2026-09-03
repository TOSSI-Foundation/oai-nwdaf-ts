/*
 * SPDX-License-Identifier: LicenseRef-CSSL-1.0
 */

/*
 * Hand-written compatibility shim, deliberately NOT in the OpenAPI-generated
 * files so a future regeneration cannot silently drop it.
 *
 * The SMF serialises the PFCP usage report inside the QOS_MON event
 * notification's "customized_data" object, but the two SMF generations spell
 * the key differently:
 *
 *   oai-cn5g-smf v2.0.0  ->  "Usage Report"   (EventNotification.cpp: j["Usage Report"])
 *   oai-cn5g-smf develop ->  "UsageReport"    (SmfEventNotification.cpp: j["UsageReport"])
 *
 * The generated CustomizedData only tags the v2.0.0 spelling, so against a
 * develop-based SMF - which Route 2 requires - every usage report decoded to a
 * zero-valued struct and was stored that way. The traffic-steering engine's
 * whole feature set derives from those volumes, so all of it read zero and the
 * congestion score never moved off its floor.
 *
 * The inner field names (SEID, UR-SEQN, Duration, NoP, Volume, Trigger) are
 * identical in both generations, so accepting either outer key is enough.
 */

package smfclient

import "encoding/json"

// UnmarshalJSON accepts either spelling of the usage-report key and normalises
// onto the single UsageReport field, so the stored BSON shape is unchanged.
func (c *CustomizedData) UnmarshalJSON(data []byte) error {
	var raw struct {
		Spaced    *CustomUsageReport `json:"Usage Report"`
		Unspaced  *CustomUsageReport `json:"UsageReport"`
		Event     string             `json:"event"`
		TimeStamp string             `json:"timeStamp"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	c.Event = raw.Event
	c.TimeStamp = raw.TimeStamp
	switch {
	case raw.Spaced != nil:
		c.UsageReport = *raw.Spaced
	case raw.Unspaced != nil:
		c.UsageReport = *raw.Unspaced
	}
	return nil
}
