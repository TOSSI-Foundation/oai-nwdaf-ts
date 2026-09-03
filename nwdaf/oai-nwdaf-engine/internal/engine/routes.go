/*
 * SPDX-License-Identifier: LicenseRef-CSSL-1.0
 */

/*
 * This file contains server routes.
 */

package engine

import (
	"net/http"
)

// ------------------------------------------------------------------------------
// NewRouter - create router for HTTP server.
func NewRouter() http.Handler {
	mux := http.NewServeMux()
	// register routes
	mux.HandleFunc(config.Routes.NumOfUe, nwPerfNumOfUe)
	mux.HandleFunc(config.Routes.SessSuccRatio, nwPerfNumOfPdu)
	mux.HandleFunc(config.Routes.UeComm, ueComm)
	mux.HandleFunc(config.Routes.UeMob, ueMob)
	mux.HandleFunc(config.Routes.NfLoad, nfLoad)
	mux.HandleFunc(config.Routes.QosSustainability, qosSustainability)
	// TS 23.288 clause 6.14 DN Performance Analytics.
	mux.HandleFunc(config.Routes.DnPerformance, dnPerformance)
	return mux
}
