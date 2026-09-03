/*
 * SPDX-License-Identifier: LicenseRef-CSSL-1.0
 */

/*
 * This is main file of the oai-nwdaf-sbi HTTP server.
 */

package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/kelseyhightower/envconfig"
	sbi "gitlab.eurecom.fr/oai/cn5g/oai-cn5g-nwdaf/components/oai-nwdaf-sbi/internal/sbi"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

type MainConfig struct {
	Server struct {
		Addr string `envconfig:"SERVER_ADDR"`
	}
}

// ------------------------------------------------------------------------------
func main() {
	// load the environment variables from the file .env
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Error loading .env file")
	}
	var config MainConfig
	err = envconfig.Process("", &config)
	if err != nil {
		log.Fatal(err.Error())
	}
	// Initialize internal package
	sbi.InitConfig()
	// Create router
	router := sbi.NewRouter()
	server := &http.Server{
		Addr:         config.Server.Addr,
		Handler:      h2c.NewHandler(router, &http2.Server{}),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
	// TS 23.288 clause 6.2.2.5 requires the NWDAF to UNSUBSCRIBE as well as
	// subscribe. Without this, every restart of this container left its
	// event-exposure subscriptions behind on the AMF/SMF and added new ones,
	// so each usage report was delivered - and stored - once per stale
	// subscription.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-stop
		log.Printf("Received %s, unsubscribing from AMF/SMF event exposure", sig)
		sbi.ShutdownSubscriptions()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(ctx)
	}()

	log.Printf("Server listening at %s", config.Server.Addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	log.Printf("oai-nwdaf-sbi stopped")
}
