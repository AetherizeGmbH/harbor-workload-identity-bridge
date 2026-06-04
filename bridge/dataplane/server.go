// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// ServerConfig assembles the HTTPS server's runtime knobs. Construction
// validates the configuration up-front so the manager.Runnable cannot be
// added without working TLS — fail-fast, not fail-on-first-request.
type ServerConfig struct {
	// ListenAddr is the host:port the server binds to. Defaults to
	// ":8443" if empty.
	ListenAddr string

	// CertFile is the path to the PEM-encoded TLS certificate. Required.
	CertFile string

	// KeyFile is the path to the PEM-encoded TLS private key. Required.
	KeyFile string

	// ClientCAFile is the path to a PEM-encoded CA bundle used to
	// authenticate plugin clients. When empty, mTLS is disabled. When
	// set, the server requires and verifies a client certificate signed
	// by one of the CAs in the bundle. mTLS is optional today (the SA
	// token in the Bearer header already authenticates the workload via
	// OIDC) but ADR-0008 leaves the door open.
	ClientCAFile string

	// Handler is the HTTP handler. Typically a mux carrying the
	// credential endpoint, /metrics, and /healthz.
	Handler http.Handler

	// ShutdownTimeout bounds graceful shutdown when ctx cancels.
	// Defaults to 10 seconds.
	ShutdownTimeout time.Duration
}

// Server is a manager.Runnable HTTPS server. The manager calls Start with
// a context tied to SIGTERM; Start blocks until the context cancels, then
// performs a graceful Shutdown bounded by ShutdownTimeout.
type Server struct {
	cfg     ServerConfig
	srv     *http.Server
	tlsConf *tls.Config

	// boundAddr is set once by Start after net.Listen resolves any `:0`
	// placeholder, then read by Addr(). The atomic.Pointer crossing
	// satisfies -race; tests that poll Addr() while Start runs would
	// otherwise race the s.srv.Addr field.
	boundAddr atomic.Pointer[string]
}

// Compile-time interface check. controller-runtime's manager.Add takes
// any Runnable; this guarantees we satisfy it.
var _ manager.Runnable = (*Server)(nil)

// NewServer validates cfg and constructs a Server. Returns an error when
// the cert/key/CA files cannot be loaded — fail at startup, not on first
// request.
func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.Handler == nil {
		return nil, errors.New("server: Handler is required")
	}
	if cfg.CertFile == "" || cfg.KeyFile == "" {
		return nil, errors.New("server: CertFile and KeyFile are required")
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8443"
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = 10 * time.Second
	}

	// Load the cert once at construction so a bad path fails fast. The
	// GetCertificate hook below re-reads on each handshake so cert-manager
	// can rotate the underlying files without a pod restart.
	if _, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile); err != nil {
		return nil, fmt.Errorf("server: load TLS keypair: %w", err)
	}

	tlsConf := &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			// Re-read on every handshake. This is cheap (a few-KB file
			// read) and means cert-manager's renewal of the mounted
			// Secret takes effect on the next handshake without
			// requiring a pod restart.
			c, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
			if err != nil {
				return nil, err
			}
			return &c, nil
		},
	}

	if cfg.ClientCAFile != "" {
		caPEM, err := os.ReadFile(cfg.ClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("server: read client CA bundle: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("server: client CA bundle %q contained no PEM-encoded certificates", cfg.ClientCAFile)
		}
		tlsConf.ClientCAs = pool
		tlsConf.ClientAuth = tls.RequireAndVerifyClientCert
	}

	s := &Server{cfg: cfg, tlsConf: tlsConf}
	s.srv = &http.Server{
		Addr:      cfg.ListenAddr,
		Handler:   cfg.Handler,
		TLSConfig: tlsConf,
		// Conservative timeouts; the credential endpoint should be
		// sub-second in steady state. Generous enough to survive a slow
		// Kubernetes API server during list().
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	return s, nil
}

// Addr returns the address the server is bound to. Before Start binds
// the listener this is the configured value (which may be ":0"); after
// Start resolves the bound port it is the concrete `127.0.0.1:NNNN`.
// Safe to call concurrently with Start — backed by atomic.Pointer to
// satisfy -race.
func (s *Server) Addr() string {
	if p := s.boundAddr.Load(); p != nil {
		return *p
	}
	return s.cfg.ListenAddr
}

// Start implements manager.Runnable. Blocks until ctx is cancelled, then
// performs a graceful Shutdown bounded by cfg.ShutdownTimeout. Returning
// from Start signals manager that this runnable is done.
func (s *Server) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("dataplane-server")

	// Bind explicitly so callers (tests, mostly) using ":0" can recover
	// the chosen port via Addr() before Serve unblocks.
	ln, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("server: listen on %s: %w", s.cfg.ListenAddr, err)
	}
	addr := ln.Addr().String()
	s.boundAddr.Store(&addr)

	errCh := make(chan error, 1)
	go func() {
		logger.Info("data-plane server listening", "addr", addr, "mtls", s.cfg.ClientCAFile != "")
		// ServeTLS with empty cert/key falls back to TLSConfig.GetCertificate.
		if err := s.srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
		defer cancel()
		logger.Info("shutting down data-plane server", "timeout", s.cfg.ShutdownTimeout)
		if err := s.srv.Shutdown(shutCtx); err != nil {
			// Drain the listener goroutine so we don't leak it on the
			// way out, even when Shutdown errors.
			<-errCh
			return fmt.Errorf("server: shutdown: %w", err)
		}
		// Drain the goroutine — ServeTLS returns ErrServerClosed which
		// our select arm treats as a clean exit.
		<-errCh
		return nil
	case err := <-errCh:
		// Listener died on its own (bind error after Serve started,
		// TLS handshake setup failure, etc.). Return so the manager
		// can shut everything down.
		return err
	}
}
