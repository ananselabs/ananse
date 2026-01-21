// pkg/proxy/handler.go
package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"strconv"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// ProxyHandler handles the incoming requests and manages retries/load balancing
type ProxyHandler struct {
	lb             *LoadBalancer
	pool           *BackendPool
	health         *Health
	proxy          *httputil.ReverseProxy
	modifyResponse func(*http.Response) error
	router         *Router
}

func NewProxyHandler(router *Router, lb *LoadBalancer, pool *BackendPool, health *Health, proxy *httputil.ReverseProxy) *ProxyHandler {
	return &ProxyHandler{
		lb:             lb,
		pool:           pool,
		health:         health,
		proxy:          proxy,
		modifyResponse: ModifyResponse(),
		router:         router,
	}
}

func (h *ProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	RecordRequestStart()
	defer RecordRequestEnd()
	startTime := time.Now()

	// Extract incoming trace context
	ctx := otel.GetTextMapPropagator().Extract(
		r.Context(),
		propagation.HeaderCarrier(r.Header),
	)

	// Start root span
	ctx, span := otel.Tracer("ananse").Start(ctx, "proxy.handle_request",
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			attribute.String("http.method", r.Method),
			attribute.String("http.url", r.URL.Path),
			attribute.String("http.host", r.Host),
		),
	)
	Logger.Info("span created", zap.String("traceID", span.SpanContext().TraceID().String()))
	defer span.End()

	// Route lookup (child span)
	_, routeSpan := otel.Tracer("ananse").Start(ctx, "proxy.route_lookup")
	serviceName, err := h.router.FindService(r)

	if err != nil {
		routeSpan.RecordError(err)
		routeSpan.SetStatus(codes.Error, "route not found")
		routeSpan.End()

		span.RecordError(err)
		span.SetStatus(codes.Error, "route not found")
		http.Error(w, "No matching route", http.StatusNotFound)
		return
	}

	routeSpan.SetAttributes(attribute.String("mesh.service", serviceName))
	routeSpan.End()

	span.SetAttributes(attribute.String("mesh.service", serviceName))

	maxRetries := 3
	var lastErr error

	for attempt := 0; attempt < maxRetries; attempt++ {
		// Load balance (child span)
		_, lbSpan := otel.Tracer("ananse").Start(ctx, "proxy.load_balance",
			trace.WithAttributes(
				attribute.Int("mesh.attempt", attempt+1),
			),
		)

		backend, err := h.lb.GetNextPeer(serviceName)

		if err != nil || backend == nil {
			lbSpan.RecordError(err)
			lbSpan.SetStatus(codes.Error, "no backend available")
			lbSpan.End()

			lastErr = err
			if attempt < maxRetries-1 {
				span.AddEvent("retry_attempt",
					trace.WithAttributes(
						attribute.Int("attempt", attempt+1),
						attribute.String("reason", "no backend available"),
					),
				)
				continue
			}

			span.RecordError(err)
			span.SetStatus(codes.Error, "no backend available")
			http.Error(w, "No backend available", http.StatusServiceUnavailable)
			return
		}

		lbSpan.SetAttributes(attribute.String("mesh.backend", backend.Name))
		lbSpan.End()

		span.SetAttributes(attribute.String("mesh.backend", backend.Name))
		backend.IncrementActiveRequests()

		// Forward request (child span)
		forwardCtx, forwardSpan := otel.Tracer("ananse").Start(ctx, "proxy.forward_request",
			trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(
				attribute.String("peer.service", serviceName),
				attribute.String("peer.address", backend.TargetUrl.String()),
				attribute.String("mesh.backend", backend.Name),
			),
		)

		// Add custom context keys
		forwardCtx = WithContextKey(forwardCtx, backendKey, backend)
		forwardCtx = WithContextKey(forwardCtx, requestTimerKey, time.Now().UTC())
		forwardCtx = WithContextKey(forwardCtx, serviceKey, serviceName)

		outReq := r.Clone(forwardCtx)

		if h.proxy.Director != nil {
			h.proxy.Director(outReq)
		}

		resp, err := h.proxy.Transport.RoundTrip(outReq)

		if err != nil {
			forwardSpan.RecordError(err)
			forwardSpan.SetStatus(codes.Error, err.Error())
			forwardSpan.End()

			RecordBackendFailure(backend.Name)
			backend.DecrementActiveRequests()
			h.pool.UpdateBackendStatus(serviceName, backend.Name, false, h.health.GetHealthCheckInterval())

			lastErr = err
			if attempt < maxRetries-1 {
				span.AddEvent("retry_attempt",
					trace.WithAttributes(
						attribute.Int("attempt", attempt+1),
						attribute.String("backend", backend.Name),
						attribute.String("reason", err.Error()),
					),
				)
				RecordRetryAttempt(backend.Name)
				continue
			}

			span.RecordError(lastErr)
			span.SetStatus(codes.Error, "all retries failed")
			http.Error(w, "All backends failed", http.StatusBadGateway)
			return
		}

		defer resp.Body.Close()

		if h.proxy.ModifyResponse != nil {
			err = h.proxy.ModifyResponse(resp)
			if err != nil {
				forwardSpan.RecordError(err)
				forwardSpan.SetStatus(codes.Error, err.Error())
				forwardSpan.End()
				backend.DecrementActiveRequests()

				lastErr = err
				if attempt < maxRetries-1 {
					span.AddEvent("retry_attempt",
						trace.WithAttributes(
							attribute.Int("attempt", attempt+1),
							attribute.String("reason", "modify_response_failed"),
						),
					)
					continue
				}

				span.RecordError(err)
				span.SetStatus(codes.Error, "modify response failed")
				http.Error(w, "Internal error", http.StatusInternalServerError)
				return
			}
		}

		// Success!
		forwardSpan.SetAttributes(
			attribute.Int("http.status_code", resp.StatusCode),
		)

		if resp.StatusCode >= 500 {
			forwardSpan.SetStatus(codes.Error, fmt.Sprintf("HTTP %d", resp.StatusCode))
		} else {
			forwardSpan.SetStatus(codes.Ok, "")
		}
		forwardSpan.End()

		// Copy response
		for key, values := range resp.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)

		backend.DecrementActiveRequests()

		duration := time.Since(startTime).Seconds()
		span.SetAttributes(
			attribute.Float64("http.duration_seconds", duration),
			attribute.Int("http.status_code", resp.StatusCode),
		)

		if resp.StatusCode >= 500 {
			span.SetStatus(codes.Error, fmt.Sprintf("HTTP %d", resp.StatusCode))
		} else if resp.StatusCode >= 400 {
			// Client errors are not span errors (it's the client's fault)
			span.SetStatus(codes.Ok, "")
		} else {
			span.SetStatus(codes.Ok, "")
		}

		RecordRequest(backend.Name, r.Method, strconv.Itoa(resp.StatusCode), duration)
		return
	}
}
