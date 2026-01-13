// pkg/proxy/reverse_proxy.go
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"go.uber.org/zap"
)

type serviceKeyType struct{}
type backendKeyType struct{}

type requestTimerKeyType struct{}

var requestTimerKey = requestTimerKeyType{}
var serviceKey = serviceKeyType{}
var backendKey = backendKeyType{}

func WithContextKey(ctx context.Context, key interface{}, val interface{}) context.Context {
	return context.WithValue(ctx, key, val)
}

func Director() func(r *http.Request) {
	return func(request *http.Request) {
		backend, ok := request.Context().Value(backendKey).(*Backend)
		if !ok {
			return
		}

		ctx := request.Context()
		target := backend.TargetUrl
		request.URL.Scheme = target.Scheme
		request.URL.Host = target.Host
		request.Host = target.Host
		request.URL.Path = singleJoiningSlash(target.Path, request.URL.Path)

		//add custom headers
		host, _, err := net.SplitHostPort(request.RemoteAddr)
		if err != nil {
			// If no port, use the whole RemoteAddr as host
			host = request.RemoteAddr
		}
		if prior := request.Header.Get("X-Forwarded-For"); prior != "" {
			request.Header.Set("X-Forwarded-For", prior+", "+host)
		} else {
			request.Header.Set("X-Forwarded-For", host)
		}
		request.Header.Set("X-Origin-Host", target.Host)
		if request.TLS == nil {
			request.Header.Set("X-Forwarded-Proto", "http")
		} else {
			request.Header.Set("X-Forwarded-Proto", "https")
		}
		request.Header.Set("X-Backend-Name", backend.Name)

		rid := request.Header.Get("X-Request-ID")
		if rid == "" {
			rid = strconv.FormatInt(time.Now().UnixNano(), 10)
			request.Header.Set("X-Request-ID", rid)
		}
		//	//log request
		reqLog := RequestLog{
			ID:         request.Header.Get("X-Request-ID"),
			Method:     request.Method,
			URL:        request.URL.String(),
			Headers:    request.Header,
			RemoteAddr: request.RemoteAddr,
			Timestamp:  time.Now().UTC(),
		}
		if startTime, ok := ctx.Value(requestTimerKey).(time.Time); ok {
			reqLog.ProcessingTime = time.Since(startTime)
			reqLog.UpstreamTime = time.Since(ctx.Value(requestTimerKey).(time.Time))
		}
		jsonLog, _ := json.Marshal(reqLog)
		Logger.Info("Request Received: ", zap.String("request", string(jsonLog)))
	}
}

func Transport() *http.Transport {
	return &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		MaxConnsPerHost:     50,
		IdleConnTimeout:     90 * time.Second,
	}
}

func ErrorHandler(bkPool *BackendPool, health *Health) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		service, _ := r.Context().Value(serviceKey).(string)
		backend, ok := r.Context().Value(backendKey).(*Backend)
		if ok {
			bkPool.UpdateBackendStatus(service, backend.Name, false, health.GetHealthCheckInterval())
			Logger.Info("proxy error,  backend down: ",
				zap.String("service", service),
				zap.String("rid", r.Header.Get("X-Request-ID")),
				zap.String("backend", backend.Name),
				zap.Error(err))
		} else {
			Logger.Info("proxy error (rid=%s): %v",
				zap.String("rid", r.Header.Get("X-Request-ID")),
				zap.Error(err))
		}

		if errors.Is(err, context.Canceled) {
			w.WriteHeader(499)
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			http.Error(w, "Gateway Timeout", http.StatusGatewayTimeout)
			return
		}
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
	}

}

func ModifyResponse() func(*http.Response) error {
	return func(response *http.Response) error {
		var bodyBytes []byte
		var err error
		if response.Request.URL.Path == "/health" {
			bodyBytes, err = io.ReadAll(response.Body)
			if err != nil {
				return err
			}
			response.Body.Close()
		}
		ctx := response.Request.Context()
		startTime, ok := ctx.Value(requestTimerKey).(time.Time)
		var duration time.Duration
		if !ok {
			Logger.Info("Warning: request-timer not found in context")
			duration = 0
		} else {
			duration = time.Since(startTime)
		}

		if response.Request.URL.Path == "/health" {
			var healthCheck struct {
				Status    string    `json:"status"`
				TimeStamp time.Time `json:"timeStamp"`
			}
			response.Header.Set("Via", "1.1 go-mini-proxy")
			response.Header.Set("X-Modified-By-Proxy", "true")

			if err := json.Unmarshal(bodyBytes, &healthCheck); err != nil {
				response.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
				return nil
			}
			currResponse := map[string]interface{}{
				"status":          healthCheck.Status, //TODO: Implement a custom proxy status
				"timestamp":       time.Now().UTC().Format(time.RFC3339),
				"original_status": healthCheck.Status,
			}
			currBody, err := json.Marshal(currResponse)
			if err != nil {
				return err
			}

			response.Body = io.NopCloser(bytes.NewReader(currBody))
			response.ContentLength = int64(len(currBody))
			response.Header.Set("Content-Length", strconv.Itoa(len(currBody)))
		}
		respLog := ResponseLog{
			Status:     response.Status,
			StatusCode: response.StatusCode,
			Headers:    response.Header,
			Timestamp:  time.Now().UTC(),
			Duration:   duration, //TODO: look at the time between when the request finished processing and when the response was sent
		}
		jsonLog, _ := json.Marshal(respLog)
		Logger.Info("Response Received: ", zap.String("response", string(jsonLog)))
		return nil
	}
}
