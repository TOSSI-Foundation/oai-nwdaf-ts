/*
 * SPDX-License-Identifier: LicenseRef-CSSL-1.0
 */

/*
 * Standards-correct error responses and a bounded HTTP client for engine calls.
 *
 * ERROR RESPONSES - what was wrong and what the specification requires
 * -----------------------------------------------------------------------------
 * TS 29.520 clause 6.1.3.2 maps the Nnwdaf_AnalyticsInfo error responses onto
 * TS 29.571 ProblemDetails, and the vendored OpenAPI for
 * GET /nnwdaf-analyticsinfo/v1/analytics declares 400/401/403/404/406/414/429/
 * 500/503 with
 *
 *     content:
 *       application/problem+json:
 *         schema: ProblemDetails
 *
 * The service layer DID build a ProblemDetails, but the generated
 * DefaultErrorHandler in error.go throws the response body away and writes
 * err.Error() instead, so every error came back as a bare JSON string with
 * Content-Type application/json - e.g. `"missing Network Performance Data"`.
 * A consumer cannot machine-read that: there is no status, no cause, no title.
 *
 * CLASSIFICATION: OAI implementation gap.
 *
 * The generated error.go is left untouched; a ProblemDetails-aware ErrorHandler
 * is injected into the controller instead, which is the extension point the
 * generator itself provides (WithNWDAFAnalyticsDocumentApiErrorHandler).
 */

package analytics

import (
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// ------------------------------------------------------------------------------
// problemError - an error that carries the ProblemDetails a consumer must see.
type problemError struct {
	status int
	// cause is the TS 29.571 "machine-readable application error cause".
	cause  string
	detail string
}

func (e *problemError) Error() string { return e.detail }

// newProblem - build a ProblemDetails-carrying error.
func newProblem(status int, cause string, detail string) error {
	return &problemError{status: status, cause: cause, detail: detail}
}

// ------------------------------------------------------------------------------
// ProblemDetailsErrorHandler - render errors as TS 29.571 ProblemDetails with
// the application/problem+json media type required by TS 29.500 clause 5.2.7.
func ProblemDetailsErrorHandler(
	w http.ResponseWriter, r *http.Request, err error, result *ImplResponse,
) {
	status := http.StatusInternalServerError
	cause := "INTERNAL_ERROR"
	detail := "unspecified error"

	switch e := err.(type) {
	case *problemError:
		status, cause, detail = e.status, e.cause, e.detail
	default:
		if result != nil && result.Code != 0 {
			status = result.Code
		}
		if err != nil {
			detail = err.Error()
		}
		if status == http.StatusBadRequest {
			cause = "MANDATORY_IE_INCORRECT"
		}
	}

	pd := ProblemDetails{
		Title:  http.StatusText(status),
		Status: int32(status),
		Detail: detail,
		Cause:  cause,
	}
	if r != nil {
		pd.Instance = r.URL.RequestURI()
	}
	body, mErr := json.Marshal(pd)
	if mErr != nil {
		http.Error(w, detail, status)
		return
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	w.Write(body)
	log.Printf("Nnwdaf_AnalyticsInfo error %d (%s): %s", status, cause, detail)
}

// ------------------------------------------------------------------------------
// engineClient - HTTP client for the PROJECT-INTERNAL NBI -> engine calls.
//
// http.DefaultClient has NO timeout, so a wedged engine blocked the handling
// goroutine indefinitely and the consumer's request never completed. A bounded
// client turns that into a 503, which is a response the consumer can act on.
// CLASSIFICATION: OAI implementation gap (robustness).
func engineClient() *http.Client {
	return &http.Client{Timeout: 15 * time.Second}
}
