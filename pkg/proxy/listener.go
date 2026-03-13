package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

const (
	OutboundPort = ":15001"
	InboundPort  = ":15006"
	AdminPort    = ":15021"
	http2Preface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"
)

var sidecarReady atomic.Bool

var httpMethods = []string{"GET ", "POST ", "PUT ", "DELETE ", "HEAD ", "OPTIONS ", "PATCH "}

type SideCarProxyState struct {
	lb          *LoadBalancer
	pool        *BackendPool
	health      *Health
	router      *Router
	tlsConfig   *tls.Config
	certWatcher *CertWatcher
}

func NewSideCarProxyState(router *Router, lb *LoadBalancer, pool *BackendPool, health *Health) *SideCarProxyState {
	return &SideCarProxyState{
		lb:     lb,
		pool:   pool,
		health: health,
		router: router,
	}
}

// dont mind me, i needed a way to pass the reader into the function as a readwriter
type readWriter struct {
	io.Reader
	io.Writer
	conn *net.TCPConn
}

func (rw readWriter) CloseWrite() error {
	if rw.conn != nil {
		return rw.conn.CloseWrite()
	}
	return nil
}

// startAdminServer starts the admin HTTP server for metrics and health checks
func startAdminServer(ctx context.Context) *http.Server {
	mux := http.NewServeMux()

	// Prometheus metrics endpoint
	mux.Handle("/metrics", promhttp.Handler())

	// Liveness probe - always healthy if process is running
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// Readiness probe - healthy when listeners are up
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		if sidecarReady.Load() {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ready"))
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("not ready"))
		}
	})

	server := &http.Server{
		Addr:    AdminPort,
		Handler: mux,
	}

	go func() {
		Logger.Info("admin server started", zap.String("port", AdminPort))
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			Logger.Error("admin server error", zap.Error(err))
		}
	}()

	return server
}

// StartSidecarProxy starts both inbound and outbound listeners
func StartSidecarProxy(state *ProxyState, shutdownCh chan os.Signal, ctx context.Context, cancel context.CancelFunc) {
	sidecarProxy := NewSideCarProxyState(state.Router, state.LoadBalancer, state.BackendPool, state.Health)

	// Start admin server for metrics/health
	adminServer := startAdminServer(ctx)
	defer adminServer.Shutdown(context.Background())

	Logger.Info("sidecar proxy started",
		zap.String("outbound", OutboundPort),
		zap.String("inbound", InboundPort),
		zap.String("admin", AdminPort))

	mtlsEnabled := os.Getenv("ANANSE_MTLS_ENABLED")

	if mtlsEnabled == "true" {
		certDir := os.Getenv("ANANSE_CERT_PATH")
		if certDir == "" {
			certDir = "/etc/ananse/certs"
		}

		certWatcher, err := NewCertWatcher(certDir)
		if err != nil {
			Logger.Fatal("failed to initialize cert watcher", zap.Error(err))
		}
		sidecarProxy.certWatcher = certWatcher
		certWatcher.Start()

		Logger.Info("mTLS enabled with automatic cert reload",
			zap.String("cert_dir", certDir))
	} else {
		Logger.Warn("mTLS disabled, connections are insecure")
	}

	go sidecarProxy.startInboundListener()
	go sidecarProxy.startOutboundListener()

	// Mark ready after listeners start
	sidecarReady.Store(true)

	select {
	case sig := <-shutdownCh:
		Logger.Info("signal received, shutting down", zap.String("signal", sig.String()))
	case <-ctx.Done():
		Logger.Info("context cancelled, shutting down")
	}
	// 3. Perform cleanup
	cancel()
	Logger.Info("sidecar proxy stopped")

}

// =====================================================================
// OUTBOUND: App -> Sidecar -> Upstream Service
// =====================================================================

func (spx *SideCarProxyState) startOutboundListener() {
	ln, err := net.Listen("tcp", OutboundPort)
	if err != nil {
		Logger.Fatal("failed to start outbound listener", zap.Error(err))
	}
	Logger.Info("outbound listener started", zap.String("port", OutboundPort))

	for {
		conn, err := ln.Accept()
		if err != nil {
			Logger.Error("failed to accept outbound connection", zap.Error(err))
			continue
		}

		tcpConn, ok := conn.(*net.TCPConn)
		if !ok {
			Logger.Error("not a TCP connection")
			conn.Close()
			continue
		}

		// Handle each connection in a goroutine
		go spx.handleOutboundConnection(tcpConn)
	}
}

func (spx *SideCarProxyState) handleOutboundConnection(clientConn *net.TCPConn) {
	defer clientConn.Close()
	RecordRequestStart()
	defer RecordRequestEnd()
	RecordSidecarConnectionStart("outbound")
	defer RecordSidecarConnectionEnd("outbound")
	_ = clientConn.SetNoDelay(true)

	startTime := time.Now()

	Logger.Info("receiving outbound traffic")

	// A. RECOVER ORIGINAL DESTINATION
	target, err := getOriginalDst(clientConn)
	if err != nil {
		Logger.Error("failed to get original destination", zap.Error(err))
		return
	}
	Logger.Info("outbound connection", zap.String("destination", target))

	// B. DETECT PROTOCOL (before service discovery)
	reader := bufio.NewReader(clientConn)
	clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	header, _ := reader.Peek(24) // ← Peek more bytes for HTTP
	clientConn.SetReadDeadline(time.Time{})
	prtDetect := spx.detectProtocol(header)
	RecordSidecarConnection("outbound", prtDetect)

	var targetRW readWriter

	switch prtDetect {
	case "HTTP":
		req, err := http.ReadRequest(reader)
		if err != nil {
			Logger.Error("failed to read HTTP request", zap.Error(err))
			return
		}

		// Extract trace context
		ctx := otel.GetTextMapPropagator().Extract(
			req.Context(),
			propagation.HeaderCarrier(req.Header),
		)

		// Create root span
		ctx, span := otel.Tracer("ananse").Start(ctx, "proxy.outbound",
			trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(
				attribute.String("http.method", req.Method),
				attribute.String("http.url", req.URL.String()),
				attribute.String("http.host", req.Host),
				attribute.String("peer.address", target),
			),
		)
		defer span.End()

		// Inject span context
		otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))

		// C. SERVICE DISCOVERY (VIP lookup)
		_, routeSpan := otel.Tracer("ananse").Start(ctx, "proxy.route_lookup")
		serviceName, err := spx.router.FindServiceByVIP(target)
		if err != nil {
			routeSpan.RecordError(err)
			routeSpan.SetStatus(codes.Error, "route not found")
			routeSpan.End()
			span.RecordError(err)
			span.SetStatus(codes.Error, "route not found")
			Logger.Error("No matching route", zap.String("target", target), zap.Error(err))
			return
		}
		routeSpan.SetAttributes(attribute.String("mesh.service", serviceName))
		routeSpan.End()
		span.SetAttributes(attribute.String("mesh.service", serviceName))

		// D. RETRY LOOP (load balance + dial + forward)
		maxRetries := 3
		var backend *Backend
		var resp *http.Response
		var lastErr error
		var targetConn net.Conn
		var targetReader *bufio.Reader

		for attempt := 0; attempt < maxRetries; attempt++ {
			// D1. LOAD BALANCE
			_, lbSpan := otel.Tracer("ananse").Start(ctx, "proxy.load_balance",
				trace.WithAttributes(
					attribute.Int("mesh.attempt", attempt+1),
				),
			)

			backend, lastErr = spx.lb.GetNextPeer(serviceName)
			if lastErr != nil || backend == nil {
				lbSpan.RecordError(lastErr)
				lbSpan.SetStatus(codes.Error, "no backend available")
				lbSpan.End()

				Logger.Error("No backend available", zap.Error(lastErr), zap.Int("attempt", attempt+1))
				if attempt < maxRetries-1 {
					span.AddEvent("retry_attempt",
						trace.WithAttributes(
							attribute.Int("attempt", attempt+1),
							attribute.String("reason", "no backend available"),
						),
					)
					continue
				}
				span.RecordError(lastErr)
				span.SetStatus(codes.Error, "no backend available")
				return
			}

			lbSpan.SetAttributes(attribute.String("mesh.backend", backend.Name))
			lbSpan.End()
			span.SetAttributes(attribute.String("mesh.backend", backend.Name))

			// D2. DIAL SELECTED BACKEND
			_, dialSpan := otel.Tracer("ananse").Start(ctx, "proxy.dial_backend",
				trace.WithAttributes(
					attribute.String("peer.address", backend.TargetUrl.Host),
				),
			)

			backend.IncrementActiveRequests()
			dialer := net.Dialer{Timeout: 2 * time.Second}

			if spx.certWatcher != nil { // ← mTLS enabled
				tlsConfig := spx.certWatcher.GetConfig() // ← Get current config
				// Skip server cert verification (pod IPs don't match DNS SANs)
				// mTLS client cert still provides mutual authentication
				tlsConfig.InsecureSkipVerify = true
				tlsConfig.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
					certs := make([]*x509.Certificate, len(rawCerts))
					for i, raw := range rawCerts {
						cert, err := x509.ParseCertificate(raw)
						if err != nil {
							return err
						}
						certs[i] = cert
					}

					// Verify cert is signed by our CA (not expired, valid chain)
					opts := x509.VerifyOptions{
						Roots:         spx.certWatcher.GetCAPool(),
						Intermediates: x509.NewCertPool(),
					}
					_, err := certs[0].Verify(opts)
					return err
				}

				targetConn, err = tls.DialWithDialer(&dialer, "tcp", backend.TargetUrl.Host, tlsConfig)
			} else { // Plain TCP
				targetConn, err = dialer.Dial("tcp", backend.TargetUrl.Host)
			}

			if err != nil {
				backend.DecrementActiveRequests()
				dialSpan.RecordError(err)
				dialSpan.SetStatus(codes.Error, err.Error())
				dialSpan.End()

				Logger.Error("failed to dial backend",
					zap.Error(err),
					zap.String("backend", backend.Name),
				)

				RecordBackendFailure(backend.Name)
				spx.pool.UpdateBackendStatus(serviceName, backend.Name, false, spx.health.GetHealthCheckInterval())

				lastErr = err
				if attempt < maxRetries-1 {
					span.AddEvent("retry_attempt",
						trace.WithAttributes(
							attribute.Int("attempt", attempt+1),
							attribute.String("backend", backend.Name),
							attribute.String("reason", "dial failed"),
						),
					)
					RecordRetryAttempt(backend.Name)
					continue
				}
				span.RecordError(lastErr)
				span.SetStatus(codes.Error, "all retries failed")
				return
			}

			if tc, ok := targetConn.(*net.TCPConn); ok {
				_ = tc.SetNoDelay(true)
			}

			dialSpan.SetStatus(codes.Ok, "")
			dialSpan.End()

			// D3. FORWARD REQUEST
			_, forwardSpan := otel.Tracer("ananse").Start(ctx, "proxy.forward_request",
				trace.WithAttributes(
					attribute.String("peer.service", serviceName),
					attribute.String("peer.address", backend.TargetUrl.Host),
				),
			)

			// Add forwarding headers
			host, _, _ := net.SplitHostPort(clientConn.RemoteAddr().String())
			if prior := req.Header.Get("X-Forwarded-For"); prior != "" {
				req.Header.Set("X-Forwarded-For", prior+", "+host)
			} else {
				req.Header.Set("X-Forwarded-For", host)
			}
			req.Header.Set("X-Forwarded-Proto", "http")

			// Ensure request ID
			rid := req.Header.Get("X-Request-ID")
			if rid == "" {
				rid = strconv.FormatInt(time.Now().UnixNano(), 10)
				req.Header.Set("X-Request-ID", rid)
			}

			requestStart := time.Now()
			if err := req.Write(targetConn); err != nil {
				targetConn.Close()
				backend.DecrementActiveRequests()
				forwardSpan.RecordError(err)
				forwardSpan.SetStatus(codes.Error, err.Error())
				forwardSpan.End()

				Logger.Error("failed to forward request", zap.Error(err))
				RecordBackendFailure(backend.Name)
				spx.pool.UpdateBackendStatus(serviceName, backend.Name, false, spx.health.GetHealthCheckInterval())

				lastErr = err
				if attempt < maxRetries-1 {
					RecordRetryAttempt(backend.Name)
					continue
				}
				span.RecordError(lastErr)
				span.SetStatus(codes.Error, "all retries failed")
				return
			}
			req.Body.Close()

			// D4. READ RESPONSE
			targetReader = bufio.NewReader(targetConn)
			resp, err = http.ReadResponse(targetReader, req)
			if err != nil {
				targetConn.Close()
				backend.DecrementActiveRequests()
				forwardSpan.RecordError(err)
				forwardSpan.SetStatus(codes.Error, err.Error())
				forwardSpan.End()

				Logger.Error("failed to read response", zap.Error(err))
				RecordBackendFailure(backend.Name)
				spx.pool.UpdateBackendStatus(serviceName, backend.Name, false, spx.health.GetHealthCheckInterval())

				lastErr = err
				if attempt < maxRetries-1 {
					RecordRetryAttempt(backend.Name)
					continue
				}
				span.RecordError(lastErr)
				span.SetStatus(codes.Error, "all retries failed")
				return
			}

			// Success!
			duration := time.Since(requestStart).Seconds()
			forwardSpan.SetAttributes(
				attribute.Int("http.status_code", resp.StatusCode),
				attribute.Float64("http.duration_seconds", duration),
			)

			if resp.StatusCode >= 500 {
				forwardSpan.SetStatus(codes.Error, fmt.Sprintf("HTTP %d", resp.StatusCode))
				RecordBackendFailure(backend.Name)
			} else {
				forwardSpan.SetStatus(codes.Ok, "")
			}
			forwardSpan.End()

			span.SetAttributes(
				attribute.Int("http.status_code", resp.StatusCode),
				attribute.Float64("http.duration_seconds", time.Since(startTime).Seconds()),
			)

			RecordRequest(backend.Name, req.Method, strconv.Itoa(resp.StatusCode), duration)

			// Got a successful response, exit retry loop
			break
		}

		// E. FORWARD RESPONSE TO CLIENT
		if resp == nil {
			// All retries failed (cleanup already done in loop)
			span.RecordError(lastErr)
			span.SetStatus(codes.Error, "all retries failed")
			return
		}

		// Success path: cleanup when function exits
		defer targetConn.Close()
		defer backend.DecrementActiveRequests()

		if err := resp.Write(clientConn); err != nil {
			Logger.Error("failed to write response to client", zap.Error(err))
			span.SetAttributes(attribute.String("error", err.Error()))
			return
		}
		resp.Body.Close()

		Logger.Info("outbound response successful")
		RecordSidecarDuration("outbound", strconv.Itoa(resp.StatusCode), time.Since(startTime).Seconds())

		// Skip bidirectional for health checks
		if req.URL.Path == "/health" || req.URL.Path == "/healthz" || req.URL.Path == "/ready" {
			return
		}

		// For keepalive/pipelining, continue proxying
		targetRW = readWriter{
			Reader: targetReader,
			Writer: targetConn,
			conn:   targetConn.(*net.TCPConn),
		}

	default:
		// Non-HTTP protocols: dial original destination
		Logger.Info("proxying non-HTTP protocol", zap.String("proto", prtDetect))

		dialer := net.Dialer{Timeout: 2 * time.Second}
		targetConn, err := dialer.Dial("tcp", target)
		if err != nil {
			Logger.Error("failed to dial target", zap.Error(err))
			return
		}
		defer targetConn.Close()

		targetRW = readWriter{
			Reader: bufio.NewReader(targetConn),
			Writer: targetConn,
			conn:   targetConn.(*net.TCPConn),
		}
	}

	// F. BIDIRECTIONAL PROXY (for remaining data)
	if err := spx.proxyBidirectional(clientConn, targetRW); err != nil {
		Logger.Error("proxy error", zap.Error(err))
	}
}

// =====================================================================
// INBOUND: Internet -> Sidecar -> App
// =====================================================================

func (spx *SideCarProxyState) startInboundListener() {
	if spx.certWatcher == nil {
		// Plain TCP listener (no mTLS)
		ln, err := net.Listen("tcp", InboundPort)
		if err != nil {
			Logger.Fatal("failed to start inbound listener", zap.Error(err))
		}
		Logger.Info("inbound listener started (no mTLS)", zap.String("port", InboundPort))

		for {
			conn, err := ln.Accept()
			if err != nil {
				Logger.Error("failed to accept inbound connection", zap.Error(err))
				continue
			}
			go spx.handleInboundConnection(conn)
		}
		return
	}

	// mTLS listener with permissive mode (handles both TLS and plain TCP)
	ln, err := net.Listen("tcp", InboundPort)
	if err != nil {
		Logger.Fatal("failed to start inbound listener", zap.Error(err))
	}

	Logger.Info("inbound mTLS listener started (permissive mode)", zap.String("port", InboundPort))

	for {
		conn, err := ln.Accept()
		if err != nil {
			Logger.Error("failed to accept inbound connection", zap.Error(err))
			continue
		}

		go spx.handleInboundWithDetection(conn)
	}
}

// handleInboundWithDetection peeks at first bytes to detect TLS vs plain TCP
func (spx *SideCarProxyState) handleInboundWithDetection(conn net.Conn) {
	// Peek first 1 byte to detect TLS
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	header := make([]byte, 1)
	n, err := conn.Read(header)
	conn.SetReadDeadline(time.Time{})

	if err != nil || n == 0 {
		Logger.Error("failed to peek connection", zap.Error(err))
		conn.Close()
		return
	}

	// Get underlying TCP conn for original destination lookup
	tcpConn, ok := conn.(*net.TCPConn)
	if !ok {
		Logger.Error("not a TCP connection")
		conn.Close()
		return
	}

	// TLS record starts with 0x16 (handshake)
	if header[0] == 0x16 {
		// TLS connection - wrap and do mTLS handshake
		replayConn := &replayConn{Conn: conn, buf: header, tcpConn: tcpConn}

		tlsConfig := spx.certWatcher.GetConfig()
		tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
		tlsConfig.MinVersion = tls.VersionTLS12

		tlsConn := tls.Server(replayConn, tlsConfig)
		if err := tlsConn.Handshake(); err != nil {
			Logger.Warn("mTLS handshake failed, closing", zap.Error(err))
			RecordTLSHandshake("failed")
			conn.Close()
			return
		}
		RecordTLSHandshake("success")
		RecordConnectionByTLS("mtls")
		spx.handleInboundConnection(tlsConn)
	} else {
		// Plain TCP (likely health probe) - proxy without TLS
		RecordConnectionByTLS("plain")
		replayConn := &replayConn{Conn: conn, buf: header, tcpConn: tcpConn}
		spx.handleInboundConnection(replayConn)
	}
}

// replayConn replays buffered bytes before reading from underlying conn
type replayConn struct {
	net.Conn
	buf     []byte
	tcpConn *net.TCPConn
}

func (r *replayConn) Read(p []byte) (int, error) {
	if len(r.buf) > 0 {
		n := copy(p, r.buf)
		r.buf = r.buf[n:]
		return n, nil
	}
	return r.Conn.Read(p)
}

// TCPConn returns the underlying TCP connection for SO_ORIGINAL_DST
func (r *replayConn) TCPConn() *net.TCPConn {
	return r.tcpConn
}

func (spx *SideCarProxyState) handleInboundConnection(clientConn net.Conn) {
	startTime := time.Now()
	RecordRequestStart()
	defer RecordRequestEnd()
	RecordSidecarConnectionStart("inbound")
	defer RecordSidecarConnectionEnd("inbound")
	defer clientConn.Close()

	if Logger == nil {
		InitLogger()
	}

	// get original destination
	var tcpConn *net.TCPConn
	if tc, ok := clientConn.(*net.TCPConn); ok {
		tcpConn = tc
	} else if tc, ok := clientConn.(*tls.Conn); ok {
		// Check if underlying is replayConn
		if rc, ok := tc.NetConn().(*replayConn); ok {
			tcpConn = rc.TCPConn()
		} else {
			tcpConn = tc.NetConn().(*net.TCPConn)
		}
	} else if rc, ok := clientConn.(*replayConn); ok {
		tcpConn = rc.TCPConn()
	}
	if tcpConn == nil {
		Logger.Error("failed to get TCP connection for original dest")
		return
	}
	origDst, err := getOriginalDst(tcpConn)
	var state tls.ConnectionState
	if tlsConn, ok := clientConn.(*tls.Conn); ok {
		state = tlsConn.ConnectionState()
	}
	if err != nil {
		Logger.Error("failed to get original destination", zap.Error(err))
		return
	}

	_, port, err := net.SplitHostPort(origDst)
	if err != nil {
		Logger.Error("failed to parse original destination", zap.Error(err))
		return
	}

	// connect to LOCAL application (no TLS)
	target := net.JoinHostPort("127.0.0.1", port)

	targetConn, err := net.DialTimeout("tcp", target, 2*time.Second)
	if err != nil {
		Logger.Error("failed to dial local app", zap.Error(err))
		return
	}
	defer targetConn.Close()

	reader := bufio.NewReader(clientConn)
	header, _ := reader.Peek(24)

	prtDetect := spx.detectProtocol(header)
	RecordSidecarConnection("inbound", prtDetect)

	rw := readWriter{
		Reader: reader,
		Writer: clientConn,
	}

	var targetRW readWriter

	switch prtDetect {

	case "HTTP":

		req, err := http.ReadRequest(reader)
		if err != nil {
			Logger.Error("failed to read HTTP request", zap.Error(err))
			return
		}

		ctx := otel.GetTextMapPropagator().Extract(
			req.Context(),
			propagation.HeaderCarrier(req.Header),
		)

		ctx, span := otel.Tracer("ananse").Start(ctx, "proxy.handle_request",
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.method", req.Method),
				attribute.String("http.url", req.URL.Path),
			),
		)
		if len(state.PeerCertificates) > 0 {
			span.SetAttributes(attribute.String("client.identity", state.PeerCertificates[0].Subject.CommonName))
		}
		defer span.End()

		otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))

		requestStart := time.Now()

		if err := req.Write(targetConn); err != nil {
			Logger.Error("failed to forward HTTP request", zap.Error(err))
			return
		}

		req.Body.Close()

		targetReader := bufio.NewReader(targetConn)

		resp, err := http.ReadResponse(targetReader, req)
		if err != nil {
			Logger.Error("failed to read HTTP response", zap.Error(err))
			return
		}

		span.SetAttributes(
			attribute.Int("http.status_code", resp.StatusCode),
			attribute.Float64("duration", time.Since(requestStart).Seconds()),
		)

		if err := resp.Write(clientConn); err != nil {
			Logger.Error("failed to write response", zap.Error(err))
			return
		}

		resp.Body.Close()
		RecordSidecarDuration("inbound", strconv.Itoa(resp.StatusCode), time.Since(startTime).Seconds())

		targetRW = readWriter{
			Reader: targetReader,
			Writer: targetConn,
		}

	default:

		targetRW = readWriter{
			Reader: bufio.NewReader(targetConn),
			Writer: targetConn,
		}
	}

	if err := spx.proxyBidirectional(rw, targetRW); err != nil {
		Logger.Error("proxy error", zap.Error(err))
	}
}

// =====================================================================
// BIDIRECTIONAL PROXY
// =====================================================================

// proxyBidirectional copies data between src and dst in both directions.
// Each goroutine half-closes its write end when done so the other side
// receives EOF and can exit cleanly, avoiding deadlocks on long-lived
// connections (gRPC, databases, etc).
func (spx *SideCarProxyState) proxyBidirectional(src, dst io.ReadWriter) error {
	done := make(chan error, 2)

	// src -> dst
	go func() {
		_, err := io.Copy(dst, src)
		if tc, ok := dst.(interface{ CloseWrite() error }); ok {
			tc.CloseWrite()
		}
		done <- err
	}()

	// dst -> src
	go func() {
		_, err := io.Copy(src, dst)
		if tc, ok := src.(interface{ CloseWrite() error }); ok {
			tc.CloseWrite()
		}
		done <- err
	}()

	err1 := <-done
	err2 := <-done

	if err1 != nil && err1 != io.EOF {
		return err1
	}
	if err2 != nil && err2 != io.EOF {
		return err2
	}
	return nil
}

func (spx *SideCarProxyState) detectProtocol(peeked []byte) string {
	if len(peeked) == 0 {
		return "RAW_TCP"
	}
	if len(peeked) >= 3 &&
		peeked[0] == 0x16 &&
		peeked[1] == 0x03 &&
		peeked[2] >= 0x01 && peeked[2] <= 0x04 {
		return "TLS"
	}

	s := string(peeked)

	// Check for HTTP/2 connection preface (standard for gRPC)
	// The preface is "PRI * HTTP/2.0..."
	if s == http2Preface {
		return "HTTP2"
	}

	// Check for common HTTP methods
	for _, m := range httpMethods {
		if strings.HasPrefix(s, m) {
			return "HTTP"
		}
	}

	return "RAW_TCP"
}

func LoadMTLSConfig() *tls.Config {
	clientCert, err := tls.LoadX509KeyPair("client.crt", "client.key")
	if err != nil {
		panic(err)
	}

	caCert, err := ioutil.ReadFile("ca.crt")
	if err != nil {
		panic(err)
	}

	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(caCert)

	return &tls.Config{
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      caPool,
		MinVersion:   tls.VersionTLS12,
	}
}
