// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"bytes"
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

	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
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

	// ReloadInterval is how often the serving key pair and the client CA
	// bundle are re-read from disk (in addition to file-change events), so
	// cert-manager rotations take effect without a restart. Defaults to
	// 10 seconds.
	ReloadInterval time.Duration
}

// Server is a manager.Runnable HTTPS server. The manager calls Start with
// a context tied to SIGTERM; Start blocks until the context cancels, then
// performs a graceful Shutdown bounded by ShutdownTimeout.
type Server struct {
	cfg  ServerConfig
	srv  *http.Server
	cert *certwatcher.CertWatcher
	ca   *caBundle // nil when mTLS is off

	// listening flips true once the listener is bound; readiness keys off
	// it so a replica that cannot serve credentials never receives them.
	listening atomic.Bool

	// boundAddr is set once by Start after net.Listen resolves any `:0`
	// placeholder, then read by Addr(). The atomic.Pointer crossing
	// satisfies -race; tests that poll Addr() while Start runs would
	// otherwise race the s.srv.Addr field.
	boundAddr atomic.Pointer[string]
}

// Compile-time interface check. controller-runtime's manager.Add takes
// any Runnable; this guarantees we satisfy it.
var _ manager.Runnable = (*Server)(nil)

// The data plane must serve on EVERY replica. A Runnable that does not
// implement LeaderElectionRunnable is started on the leader only, so with
// the default two replicas the follower never bound :8443 while passing
// readiness — the Service sent half of all credential requests to a closed
// port (audit H1).
var _ manager.LeaderElectionRunnable = (*Server)(nil)

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (s *Server) NeedLeaderElection() bool { return false }

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

	if cfg.ReloadInterval <= 0 {
		cfg.ReloadInterval = 10 * time.Second
	}

	// The watcher loads the pair once here (a bad path fails fast) and
	// then keeps a cached copy fresh on file events and on a timer. It
	// only swaps in a cert and key that parse as a MATCHING pair, so a
	// Secret-volume update caught half-way (new cert, old key) never
	// produces a failed handshake; and handshakes no longer cost two file
	// reads each on an unauthenticated, NodePort-reachable listener.
	cw, err := certwatcher.New(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("server: load TLS keypair: %w", err)
	}
	cw = cw.WithWatchInterval(cfg.ReloadInterval)

	tlsConf := &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: cw.GetCertificate,
	}

	var ca *caBundle
	if cfg.ClientCAFile != "" {
		ca = &caBundle{path: cfg.ClientCAFile}
		if err := ca.reload(); err != nil {
			return nil, fmt.Errorf("server: %w", err)
		}
		tlsConf.ClientAuth = tls.RequireAndVerifyClientCert
		// The client CA is resolved per handshake from the reloadable
		// bundle, so a rotated issuing CA is honoured without a restart.
		base := tlsConf.Clone()
		tlsConf.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
			c := base.Clone()
			c.ClientCAs = ca.pool.Load()
			return c, nil
		}
		tlsConf.ClientCAs = ca.pool.Load()
	}

	s := &Server{cfg: cfg, cert: cw, ca: ca}
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

	// Keep the serving pair and the client CA fresh for the lifetime of
	// the listener.
	reloadCtx, stopReload := context.WithCancel(ctx)
	defer stopReload()
	go func() {
		if err := s.cert.Start(reloadCtx); err != nil {
			logger.Error(err, "certificate watcher stopped")
		}
	}()
	if s.ca != nil {
		go s.ca.watch(reloadCtx, s.cfg.ReloadInterval, logger.Error)
	}
	s.listening.Store(true)
	defer s.listening.Store(false)

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

// ReadyCheck is a healthz.Checker: ready once the credential listener is
// bound. Wire it into the manager's readyz so a replica that cannot serve
// never receives traffic.
func (s *Server) ReadyCheck(_ *http.Request) error {
	if !s.listening.Load() {
		return errors.New("data-plane listener not bound")
	}
	return nil
}

// caBundle is a client-CA pool that can be reloaded from disk. The last
// good pool stays in use when a reload reads an empty or unparseable file
// (e.g. mid-rotation).
type caBundle struct {
	path string
	pem  []byte
	pool atomic.Pointer[x509.CertPool]
}

func (c *caBundle) reload() error {
	raw, err := os.ReadFile(c.path)
	if err != nil {
		return fmt.Errorf("read client CA bundle: %w", err)
	}
	if c.pool.Load() != nil && bytes.Equal(raw, c.pem) {
		return nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(raw) {
		return fmt.Errorf("client CA bundle %q contained no PEM-encoded certificates", c.path)
	}
	c.pem = raw
	c.pool.Store(pool)
	return nil
}

func (c *caBundle) watch(ctx context.Context, interval time.Duration, logErr func(error, string, ...any)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.reload(); err != nil {
				logErr(err, "client CA reload failed; keeping the previous bundle")
			}
		}
	}
}
