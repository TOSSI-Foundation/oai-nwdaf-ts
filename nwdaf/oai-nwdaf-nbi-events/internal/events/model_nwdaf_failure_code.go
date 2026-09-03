/*
 * SPDX-License-Identifier: LicenseRef-CSSL-1.0
 */

/*
 * NwdafFailureCode - TS 29.520 Nnwdaf_EventsSubscription, schema NwdafFailureCode.
 *
 * WHY THIS FILE IS HAND-WRITTEN
 * The OpenAPI generator collapsed this schema (a string enum expressed as an
 * anyOf) into an EMPTY Go struct, exactly as it did for NfType:
 *
 *     type NwdafFailureCode struct {}
 *
 * FailureEventInfo.failureCode is a REQUIRED property, so any attempt to report
 * a per-event subscription failure would have serialised `"failureCode": {}` -
 * a required field carrying no value. That made the specification's own
 * mechanism for rejecting an individual event unusable, which is why
 * unsupported events were previously accepted and then silently ignored.
 *
 * CLASSIFICATION: OAI implementation gap (code-generation artefact). Restored to
 * the string enumeration the specification defines - a faithful representation,
 * not an extension.
 */

package events

type NwdafFailureCode string

// List of NwdafFailureCode (TS 29.520)
const (
	// UNAVAILABLE_DATA: the requested statistics information for the event is
	// rejected since necessary data to perform the service is unavailable.
	NWDAFFAILURECODE_UNAVAILABLE_DATA NwdafFailureCode = "UNAVAILABLE_DATA"
	// BOTH_STAT_PRED_NOT_ALLOWED: the start time is in the past and the end
	// time in the future, i.e. both statistics and prediction were requested.
	NWDAFFAILURECODE_BOTH_STAT_PRED_NOT_ALLOWED NwdafFailureCode = "BOTH_STAT_PRED_NOT_ALLOWED"
	// UNSATISFIED_REQUESTED_ANALYTICS_TIME: the analytics information is not
	// ready at the time indicated by the consumer.
	NWDAFFAILURECODE_UNSATISFIED_REQUESTED_ANALYTICS_TIME NwdafFailureCode = "UNSATISFIED_REQUESTED_ANALYTICS_TIME"
	// OTHER: any other reason not covered above.
	NWDAFFAILURECODE_OTHER NwdafFailureCode = "OTHER"
)

// AssertNwdafFailureCodeRequired checks if the required fields are not zero-ed
func AssertNwdafFailureCodeRequired(obj NwdafFailureCode) error {
	return nil
}

// AssertRecurseNwdafFailureCodeRequired recursively checks if required fields are not zero-ed in a nested slice.
// Accepts only nested slice of NwdafFailureCode (e.g. [][]NwdafFailureCode), otherwise ErrTypeAssertionError is thrown.
func AssertRecurseNwdafFailureCodeRequired(objSlice interface{}) error {
	return AssertRecurseInterfaceRequired(objSlice, func(obj interface{}) error {
		aNwdafFailureCode, ok := obj.(NwdafFailureCode)
		if !ok {
			return ErrTypeAssertionError
		}
		return AssertNwdafFailureCodeRequired(aNwdafFailureCode)
	})
}
