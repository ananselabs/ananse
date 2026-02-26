package proxy

import (
	"fmt"
	"io"
	"net"
	"os"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

const (
	OutboundPort = ":15001"
	InboundPort  = ":15006"
)

// StartSidecarProxy starts both inbound and outbound listeners
func StartSidecarProxy() {
	podName := os.Getenv("POD_NAME")
	if podName == "" {
		podName = fmt.Sprintf("proxy-%s", uuid.New().String()[:8])
	}
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
	defer clientConn.Close()

	// Get original destination from SO_ORIGINAL_DST
	// iptables REDIRECT preserves the original dest, so we recover it here
	origDst, err := getOriginalDst(clientConn)
	if err != nil {
		Logger.Error("failed to get original destination", zap.Error(err))
		return
	}

	// Extract port from original destination and forward to localhost
	_, port, err := net.SplitHostPort(origDst)
	if err != nil {
		Logger.Error("failed to parse original destination", zap.String("origDst", origDst), zap.Error(err))
		return
	}
	target := net.JoinHostPort("127.0.0.1", port)

	Logger.Info("inbound connection", zap.String("target", target))

	targetConn, err := net.Dial("tcp", target)
	if err != nil {
		Logger.Error("failed to dial app",
			zap.String("target", target),
			zap.Error(err))
		return
	}
	defer targetConn.Close()

	if err := proxyBidirectional(clientConn, targetConn); err != nil {
		Logger.Error("proxy error", zap.Error(err))
	}
}

// =====================================================================
// BIDIRECTIONAL PROXY
// =====================================================================

// proxyBidirectional copies data between src and dst in both directions
// Waits for both directions to complete before returning
func proxyBidirectional(src, dst net.Conn) error {
	done := make(chan error, 2)

	// Copy src -> dst
	go func() {
		_, err := io.Copy(dst, src)
		if err != nil && err != io.EOF {
			done <- err
			return
		}
		done <- nil
	}()

	// Copy dst -> src
	go func() {
		_, err := io.Copy(src, dst)
		if err != nil && err != io.EOF {
			done <- err
			return
		}
		done <- nil
	}()

	// Wait for both to complete
	err1 := <-done
	err2 := <-done

	// Close both sides gracefully
	if tc, ok := src.(*net.TCPConn); ok {
		tc.CloseRead()
	}
	if tc, ok := dst.(*net.TCPConn); ok {
		tc.CloseWrite()
	}

	if err1 != nil {
		return err1
	}
	return err2
}
