package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

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
	http2Preface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"
)

var httpMethods = []string{"GET ", "POST ", "PUT ", "DELETE ", "HEAD ", "OPTIONS ", "PATCH "}

type SideCarProxyState struct {
	lb     *LoadBalancer
	pool   *BackendPool
	health *Health
	router *Router
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

// StartSidecarProxy starts both inbound and outbound listeners
func StartSidecarProxy(state *ProxyState, shutdownCh chan os.Signal, ctx context.Context, cancel context.CancelFunc) {
	sidecarProxy := NewSideCarProxyState(state.Router, state.LoadBalancer, state.BackendPool, state.Health)

	Logger.Info("sidecar proxy started",
		zap.String("outbound", OutboundPort),
		zap.String("inbound", InboundPort))
	go sidecarProxy.startInboundListener()
	go sidecarProxy.startOutboundListener()
	select {
	case sig := <-shutdownCh:
		Logger.Info("signal received, shutting down", zap.String("signal", sig.String()))
	case <-ctx.Done():
		Logger.Info("context cancelled, shutting down")
	}
	// 3. Perform cleanup
	cancel()
	// If your sidecarProxy has a .Stop() method, call it here
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
	_ = clientConn.SetNoDelay(true)

	startTime := time.Now() // ← Add this

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
					attribute.String("peer.address", backend.TargetUrl.Host), // ✅ Host:port
				),
			)

			backend.IncrementActiveRequests()

			dialer := net.Dialer{Timeout: 2 * time.Second}
			targetConn, err = dialer.Dial("tcp", backend.TargetUrl.Host)
			if err != nil {
				backend.DecrementActiveRequests() // Cleanup failed attempt
				dialSpan.RecordError(err)
				dialSpan.SetStatus(codes.Error, err.Error())
				dialSpan.End()

				Logger.Error("failed to dial backend", zap.Error(err), zap.String("backend", backend.Name))
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
					attribute.String("peer.address", backend.TargetUrl.Host), // ✅
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
	ln, err := net.Listen("tcp", InboundPort)
	if err != nil {
		Logger.Fatal("failed to start inbound listener", zap.Error(err))
	}
	Logger.Info("inbound listener started", zap.String("port", InboundPort))

	for {
		conn, err := ln.Accept()
		if err != nil {
			Logger.Error("failed to accept inbound connection", zap.Error(err))
			continue
		}

		tcpConn, ok := conn.(*net.TCPConn)
		if !ok {
			Logger.Error("not a TCP connection")
			conn.Close()
			continue
		}

		go spx.handleInboundConnection(tcpConn)
	}
}

func (spx *SideCarProxyState) handleInboundConnection(clientConn *net.TCPConn) {
	RecordRequestStart()
	defer RecordRequestEnd()
	defer clientConn.Close()
	_ = clientConn.SetNoDelay(true)
	if Logger == nil {
		InitLogger()
	}

	type dialResult struct {
		conn net.Conn
		err  error
	}
	dialChan := make(chan dialResult, 1)

	origDst, err := getOriginalDst(clientConn)
	if err != nil {
		Logger.Error("failed to get original destination", zap.Error(err))
		return
	}

	_, port, err := net.SplitHostPort(origDst)
	if err != nil {
		Logger.Error("failed to parse original destination", zap.Error(err))
		return
	}
	target := net.JoinHostPort("127.0.0.1", port)

	go func() {
		dialer := net.Dialer{Timeout: 2 * time.Second}
		conn, err := dialer.Dial("tcp", target)
		dialChan <- dialResult{conn: conn, err: err}
	}()

	// peek while dialing to hide latency
	reader := bufio.NewReader(clientConn)
	header, _ := reader.Peek(24)
	prtDetect := spx.detectProtocol(header)

	result := <-dialChan
	if result.err != nil {
		Logger.Error("failed to dial app", zap.Error(result.err))
		return
	}
	targetConn := result.conn
	defer targetConn.Close()
	if tc, ok := targetConn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}

	// build readWriters — initialized here so both branches can assign targetRW
	rw := readWriter{
		Reader: reader,
		Writer: clientConn,
		conn:   clientConn,
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
				attribute.String("http.host", req.Host),
			),
		)
		Logger.Info("span created", zap.String("traceID", span.SpanContext().TraceID().String()))
		defer span.End()

		// inject span context into forwarded request so upstream can attach to this trace
		otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
		Logger.Info("HTTP match", zap.String("path", req.URL.Path))

		requestStart := time.Now()
		if err := req.Write(targetConn); err != nil {
			Logger.Error("failed to forward HTTP request", zap.Error(err))
			span.SetAttributes(attribute.String("error", err.Error()))
			return
		}
		req.Body.Close()

		// intercept response for observability before forwarding to client
		targetReader := bufio.NewReader(targetConn)
		resp, err := http.ReadResponse(targetReader, req)
		if err != nil {
			Logger.Error("failed to read HTTP response", zap.Error(err))
			span.SetAttributes(attribute.String("error", err.Error()))
			return
		}

		span.SetAttributes(
			attribute.Int("http.status_code", resp.StatusCode),
			attribute.Float64("req.duration", time.Since(requestStart).Seconds()),
		)
		Logger.Info("HTTP response",
			zap.Int("status", resp.StatusCode),
			zap.Float64("duration_s", time.Since(requestStart).Seconds()),
		)

		if err := resp.Write(clientConn); err != nil {
			Logger.Error("failed to write HTTP response to client", zap.Error(err))
			span.SetAttributes(attribute.String("error", err.Error()))
			return
		}
		if err := resp.Body.Close(); err != nil {
			Logger.Error("failed to close response body", zap.Error(err))
			span.SetAttributes(attribute.String("error", err.Error()))
			return
		}

		Logger.Info("Response successful", zap.String("connection", clientConn.RemoteAddr().String()))

		// Health checks are single request/response - skip bidirectional proxy
		if req.URL.Path == "/health" || req.URL.Path == "/healthz" || req.URL.Path == "/ready" {
			return
		}

		// targetReader may have buffered bytes (keepalive / pipelined requests)
		// so wrap it rather than using targetConn directly
		targetRW = readWriter{
			Reader: targetReader,
			Writer: targetConn,
			conn:   targetConn.(*net.TCPConn),
		}

	default:
		Logger.Info("proxying protocol", zap.String("proto", prtDetect))
		targetRW = readWriter{
			Reader: bufio.NewReader(targetConn),
			Writer: targetConn,
			conn:   targetConn.(*net.TCPConn),
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
