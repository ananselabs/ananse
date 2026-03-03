package proxy

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
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
func StartSidecarProxy() {

	go startOutboundListener()
	go startInboundListener()

	Logger.Info("sidecar proxy started",
		zap.String("outbound", OutboundPort),
		zap.String("inbound", InboundPort))

	// Block forever
	select {}
}

// =====================================================================
// OUTBOUND: App -> Sidecar -> Upstream Service
// =====================================================================

func startOutboundListener() {
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
		go handleOutboundConnection(tcpConn)
	}
}

func handleOutboundConnection(clientConn *net.TCPConn) {
	defer clientConn.Close()

	// A. RECOVER ORIGINAL DESTINATION
	// "Where did the app actually want to go?"
	destAddr, err := getOriginalDst(clientConn)
	if err != nil {
		Logger.Error("failed to get original destination", zap.Error(err))
		return
	}
	Logger.Info("outbound connection", zap.String("destination", destAddr))

	// B. CONNECT TO UPSTREAM
	targetConn, err := net.Dial("tcp", destAddr)
	if err != nil {
		Logger.Error("failed to connect upstream",
			zap.String("destination", destAddr),
			zap.Error(err))
		return
	}
	defer targetConn.Close()

	// C. PROXY DATA (Bidirectional Copy)
	if err := proxyBidirectional(clientConn, targetConn); err != nil {
		Logger.Error("proxy error", zap.Error(err))
	}
}

// =====================================================================
// INBOUND: Internet -> Sidecar -> App
// =====================================================================

func startInboundListener() {
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

		go handleInboundConnection(tcpConn)
	}
}

func handleInboundConnection(clientConn *net.TCPConn) {
	RecordRequestStart()
	defer RecordRequestEnd()
	defer clientConn.Close()
	_ = clientConn.SetNoDelay(true)

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
	prtDetect := detectProtocol(header)

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

		// targetReader may have buffered bytes (keepalive / pipelined requests)
		// so wrap it rather than using targetConn directly
		targetRW = readWriter{
			Reader: targetReader,
			Writer: targetConn,
			conn:   targetConn.(*net.TCPConn),
		}

	default:
		Logger.Info("proxying protocol", zap.String("proto", prtDetect))
	}

	if err := proxyBidirectional(rw, targetRW); err != nil {
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
func proxyBidirectional(src, dst io.ReadWriter) error {
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

func detectProtocol(peeked []byte) string {
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
