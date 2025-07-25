// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package httpd // import "miniflux.app/v2/internal/http/server"

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"miniflux.app/v2/internal/api"
	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/fever"
	"miniflux.app/v2/internal/googlereader"
	"miniflux.app/v2/internal/http/request"
	"miniflux.app/v2/internal/storage"
	"miniflux.app/v2/internal/ui"
	"miniflux.app/v2/internal/version"
	"miniflux.app/v2/internal/worker"

	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

func StartWebServer(store *storage.Storage, pool *worker.Pool) *http.Server {
	certFile := config.Opts.CertFile()
	keyFile := config.Opts.CertKeyFile()
	certDomain := config.Opts.CertDomain()
	listenAddr := config.Opts.ListenAddr()
	server := &http.Server{
		ReadTimeout:  time.Duration(config.Opts.HTTPServerTimeout()) * time.Second,
		WriteTimeout: time.Duration(config.Opts.HTTPServerTimeout()) * time.Second,
		IdleTimeout:  time.Duration(config.Opts.HTTPServerTimeout()) * time.Second,
		Handler:      setupHandler(store, pool),
	}

	switch {
	case os.Getenv("LISTEN_PID") == strconv.Itoa(os.Getpid()):
		startSystemdSocketServer(server)
	case strings.HasPrefix(listenAddr, "/"):
		startUnixSocketServer(server, listenAddr)
	case certDomain != "":
		config.Opts.HTTPS = true
		startAutoCertTLSServer(server, certDomain, store)
	case certFile != "" && keyFile != "":
		config.Opts.HTTPS = true
		server.Addr = listenAddr
		startTLSServer(server, certFile, keyFile)
	default:
		server.Addr = listenAddr
		startHTTPServer(server)
	}

	return server
}

func startSystemdSocketServer(server *http.Server) {
	go func() {
		f := os.NewFile(3, "systemd socket")
		listener, err := net.FileListener(f)
		if err != nil {
			printErrorAndExit(`Unable to create listener from systemd socket: %v`, err)
		}

		slog.Info(`Starting server using systemd socket`)
		if err := server.Serve(listener); err != http.ErrServerClosed {
			printErrorAndExit(`Server failed to start: %v`, err)
		}
	}()
}

func startUnixSocketServer(server *http.Server, socketFile string) {
	os.Remove(socketFile)

	go func(sock string) {
		listener, err := net.Listen("unix", sock)
		if err != nil {
			printErrorAndExit(`Server failed to start: %v`, err)
		}
		defer listener.Close()

		if err := os.Chmod(sock, 0666); err != nil {
			printErrorAndExit(`Unable to change socket permission: %v`, err)
		}

		slog.Info("Starting server using a Unix socket", slog.String("socket", sock))
		if err := server.Serve(listener); err != http.ErrServerClosed {
			printErrorAndExit(`Server failed to start: %v`, err)
		}
	}(socketFile)
}

func startAutoCertTLSServer(server *http.Server, certDomain string, store *storage.Storage) {
	server.Addr = ":https"
	certManager := autocert.Manager{
		Cache:      storage.NewCertificateCache(store),
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(certDomain),
	}
	server.TLSConfig.GetCertificate = certManager.GetCertificate
	server.TLSConfig.NextProtos = []string{"h2", "http/1.1", acme.ALPNProto}

	// Handle http-01 challenge.
	s := &http.Server{
		Handler: certManager.HTTPHandler(nil),
		Addr:    ":http",
	}
	go s.ListenAndServe()

	go func() {
		slog.Info("Starting TLS server using automatic certificate management",
			slog.String("listen_address", server.Addr),
			slog.String("domain", certDomain),
		)
		if err := server.ListenAndServeTLS("", ""); err != http.ErrServerClosed {
			printErrorAndExit(`Server failed to start: %v`, err)
		}
	}()
}

func startTLSServer(server *http.Server, certFile, keyFile string) {
	go func() {
		slog.Info("Starting TLS server using a certificate",
			slog.String("listen_address", server.Addr),
			slog.String("cert_file", certFile),
			slog.String("key_file", keyFile),
		)
		if err := server.ListenAndServeTLS(certFile, keyFile); err != http.ErrServerClosed {
			printErrorAndExit(`Server failed to start: %v`, err)
		}
	}()
}

func startHTTPServer(server *http.Server) {
	go func() {
		slog.Info("Starting HTTP server",
			slog.String("listen_address", server.Addr),
		)
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			printErrorAndExit(`Server failed to start: %v`, err)
		}
	}()
}

func setupHandler(store *storage.Storage, pool *worker.Pool) *mux.Router {
	livenessProbe := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}
	readinessProbe := func(w http.ResponseWriter, r *http.Request) {
		if err := store.Ping(); err != nil {
			http.Error(w, fmt.Sprintf("Database Connection Error: %q", err), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}

	router := mux.NewRouter()

	// These routes do not take the base path into consideration and are always available at the root of the server.
	router.HandleFunc("/liveness", livenessProbe).Name("liveness")
	router.HandleFunc("/healthz", livenessProbe).Name("healthz")
	router.HandleFunc("/readiness", readinessProbe).Name("readiness")
	router.HandleFunc("/readyz", readinessProbe).Name("readyz")

	var subrouter *mux.Router
	if config.Opts.BasePath() != "" {
		subrouter = router.PathPrefix(config.Opts.BasePath()).Subrouter()
	} else {
		subrouter = router.NewRoute().Subrouter()
	}

	if config.Opts.HasMaintenanceMode() {
		subrouter.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(config.Opts.MaintenanceMessage()))
			})
		})
	}

	subrouter.Use(middleware)

	fever.Serve(subrouter, store)
	googlereader.Serve(subrouter, store)
	api.Serve(subrouter, store, pool)
	ui.Serve(subrouter, store, pool)

	subrouter.HandleFunc("/healthcheck", readinessProbe).Name("healthcheck")

	subrouter.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(version.Version))
	}).Name("version")

	if config.Opts.HasMetricsCollector() {
		subrouter.Handle("/metrics", promhttp.Handler()).Name("metrics")
		subrouter.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				route := mux.CurrentRoute(r)

				// Returns a 404 if the client is not authorized to access the metrics endpoint.
				if route.GetName() == "metrics" && !isAllowedToAccessMetricsEndpoint(r) {
					slog.Warn("Authentication failed while accessing the metrics endpoint",
						slog.String("client_ip", request.ClientIP(r)),
						slog.String("client_user_agent", r.UserAgent()),
						slog.String("client_remote_addr", r.RemoteAddr),
					)
					http.NotFound(w, r)
					return
				}

				next.ServeHTTP(w, r)
			})
		})
	}

	return router
}

func isAllowedToAccessMetricsEndpoint(r *http.Request) bool {
	clientIP := request.ClientIP(r)

	if config.Opts.MetricsUsername() != "" && config.Opts.MetricsPassword() != "" {
		username, password, authOK := r.BasicAuth()
		if !authOK {
			slog.Warn("Metrics endpoint accessed without authentication header",
				slog.Bool("authentication_failed", true),
				slog.String("client_ip", clientIP),
				slog.String("client_user_agent", r.UserAgent()),
				slog.String("client_remote_addr", r.RemoteAddr),
			)
			return false
		}

		if username == "" || password == "" {
			slog.Warn("Metrics endpoint accessed with empty username or password",
				slog.Bool("authentication_failed", true),
				slog.String("client_ip", clientIP),
				slog.String("client_user_agent", r.UserAgent()),
				slog.String("client_remote_addr", r.RemoteAddr),
			)
			return false
		}

		if username != config.Opts.MetricsUsername() || password != config.Opts.MetricsPassword() {
			slog.Warn("Metrics endpoint accessed with invalid username or password",
				slog.Bool("authentication_failed", true),
				slog.String("client_ip", clientIP),
				slog.String("client_user_agent", r.UserAgent()),
				slog.String("client_remote_addr", r.RemoteAddr),
			)
			return false
		}
	}

	remoteIP := request.FindRemoteIP(r)
	if remoteIP == "@" {
		// This indicates a request sent via a Unix socket, always consider these trusted.
		return true
	}

	for _, cidr := range config.Opts.MetricsAllowedNetworks() {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			slog.Error("Metrics endpoint accessed with invalid CIDR",
				slog.Bool("authentication_failed", true),
				slog.String("client_ip", clientIP),
				slog.String("client_user_agent", r.UserAgent()),
				slog.String("client_remote_addr", r.RemoteAddr),
				slog.String("cidr", cidr),
			)
			return false
		}

		// We use r.RemoteAddr in this case because HTTP headers like X-Forwarded-For can be easily spoofed.
		// The recommendation is to use HTTP Basic authentication.
		if network.Contains(net.ParseIP(remoteIP)) {
			return true
		}
	}

	return false
}

func printErrorAndExit(format string, a ...any) {
	message := fmt.Sprintf(format, a...)
	slog.Error(message)
	fmt.Fprintf(os.Stderr, "%v\n", message)
	os.Exit(1)
}
