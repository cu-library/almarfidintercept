// Copyright 2023 Carleton University Library All rights reserved.
// Use of this source code is governed by the MIT
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cu-library/overridefromenv"
)

// A version flag, which should be overwritten when building using ldflags.
var version = "devel"

const (
	// EnvPrefix is the prefix for environment variables which override unset flags.
	EnvPrefix string = "ALMA_RFID_INTERCEPT"

	// DefaultAddress is the default address this proxy will listen on.
	DefaultAddress string = ":53535"

	// DefaultProxy is the default address we are proxying.
	DefaultProxy string = "http://localhost:21645"

	// DefaultOrigin is the default origin this proxy will allow CORS requests from.
	// Effectively, this is your Alma domain.
	DefaultOrigin string = "https://ocul-crl.alma.exlibrisgroup.com"
)

// ServeProxy returns a simple status OK if the server is up.
func ServeProxy(origin, proxy string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Headers", "SOAPAction,X-CustomHeader,Keep-Alive,User-Agent,X-Requested-With,If-Modified-Since,Cache-Control,Content-Type")
			if r.Method == "OPTIONS" {
				w.Header().Set("Access-Control-Allow-Private-Network", "true")
				w.Header().Set("Access-Control-Max-Age", "1728000")
				w.Header().Set("Content-Type", "text/plain charset=UTF-8")
				http.Error(w, "", http.StatusNoContent)
				return
			}
		}
		// Build the auth headers and send a request to the Summon API.
		client := new(http.Client)

		// Add a timeout to the http client.
		client.Timeout = 5 * time.Second

		// Build the API Request.
		proxyURL, err := url.Parse(proxy)
		if err != nil {
			// This should never happen, since we already parsed in main.
			http.Error(w, "Bad internal proxy address", http.StatusInternalServerError)
			return
		}
		proxyURL.Path = r.URL.Path
		proxyURL.RawQuery = r.URL.RawQuery

		// Create the request struct.
		proxyRequest, err := http.NewRequest("GET", proxyURL.String(), nil)
		if err != nil {
			http.Error(w, "Unable to build API Request.", http.StatusInternalServerError)
			return
		}

		// Close the connection after sending the request.
		proxyRequest.Close = true

		// Send the request.
		proxyResp, err := client.Do(proxyRequest)
		if err != nil {
			http.Error(w, fmt.Sprintf("Error sending API Request: %v", err), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(proxyResp.StatusCode)
		io.Copy(w, proxyResp.Body)
		proxyResp.Body.Close()
	}
}

func main() {
	// Define the command line flags.
	addr := flag.String("address", DefaultAddress, "Address to bind on.")
	proxy := flag.String("proxy", DefaultProxy, "Address we are proxying.")
	origin := flag.String("origin", DefaultOrigin, "The allowed origin for CORS. To allow any origin to connect, use '*'.")

	// Define the Usage function, which prints to Stderr
	// helpful information about the tool.
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "almarfidintercept:\n")
		fmt.Fprintf(os.Stderr, "Version %v\n", version)
		fmt.Fprintf(flag.CommandLine.Output(), "Compiled with %v\n", runtime.Version())
		flag.PrintDefaults()
		fmt.Fprintln(os.Stderr, "  Environment variables read when flag is unset:")

		flag.VisitAll(func(f *flag.Flag) {
			fmt.Fprintf(os.Stderr, "  %v%v\n", EnvPrefix, strings.ToUpper(f.Name))
		})
	}

	// Process the flags.
	flag.Parse()

	// If any flags have not been set, see if there are
	// environment variables that set them.
	err := overridefromenv.Override(flag.CommandLine, EnvPrefix)
	if err != nil {
		log.Fatalln(err)
	}

	log.Printf("Serving on address: %v\n", *addr)
	log.Printf("Allowed origin: %v\n", *origin)

	// Use an explicit request multiplexer.
	mux := http.NewServeMux()
	mux.HandleFunc("/", ServeProxy(*origin, *proxy))

	server := http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Keep track of child goroutines.
	var running sync.WaitGroup

	// Graceful shutdown on SIGINT or SIGTERM.
	shutdown := make(chan struct{})

	// Ungraceful shutdown on internal error.
	errshutdown := make(chan struct{})

	// Run a goroutine to respond to SIGINT and SIGTERM signals.
	running.Add(1)
	go func() {
		defer running.Done()
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		select {
		case <-sigs:
			err := server.Shutdown(context.Background())
			if err != nil {
				log.Printf("Error shutting down server, %v.\n", err)
			}
			close(shutdown)
		case <-errshutdown:
		}
	}()

	log.Println("Starting server.")
	err = server.ListenAndServe()
	// ListenAndServe() always returns a non-nil error.
	// The expected error here is ErrServerClosed, which is
	// returned when Shutdown() is called after SIGINT or SIGTERM
	// are captured.
	if !errors.Is(err, http.ErrServerClosed) {
		log.Printf("FATAL: Server error, %v.\n", err)
		close(errshutdown)
		running.Wait()
		os.Exit(1)
	}

	// Wait for subprocesses to exit.
	// Since ListenAndServe() returned ErrServerClosed,
	// Shutdown() was called from the signal handler above.
	// That handler will wait for Shutdown() to return.
	// Then, it will close the shutdown channel and exit,
	// which also causes the SIGHUP handler to exit.
	// When the two handlers exit, the waitgroup counter will be zero,
	// and the call to Wait() will stop blocking.
	running.Wait()
	log.Println("Server stopped.")
}
