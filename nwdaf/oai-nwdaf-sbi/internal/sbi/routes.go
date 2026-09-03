/*
 * SPDX-License-Identifier: LicenseRef-CSSL-1.0
 */

/*
 * This file contains server routes.
 */

package sbi

import (
	"net/http"
)

// ------------------------------------------------------------------------------
// NewRouter - create router for HTTP server.
func NewRouter() http.Handler {
	mux := http.NewServeMux()
	// register routes
	mux.HandleFunc(config.Amf.ApiRoute, storeAmfNotificationOnDB)
	mux.HandleFunc(config.Smf.ApiRoute, storeSmfNotificationOnDB)
	return mux
}
