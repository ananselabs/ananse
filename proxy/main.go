package main

import (
	lb "ananse/pkg/proxy"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

func main() {

	backends := []*lb.Backend{
		{Name: "backend1", TargetUrl: mustParse("http://localhost:5004"), Healthy: true},
		{Name: "backend2", TargetUrl: mustParse("http://localhost:5001"), Healthy: true},
		{Name: "backend3", TargetUrl: mustParse("http://localhost:5003"), Healthy: true},
		{Name: "backend4", TargetUrl: mustParse("http://localhost:5002"), Healthy: true},
	}
	// create a reverse proxy

	bkPool := lb.NewBackendPool(backends, "least-connections", 3*time.Second)
	proxy := &httputil.ReverseProxy{
		Director: func(request *http.Request) {
			backend, ok := request.Context().Value("backend").(*lb.Backend)
			if !ok {
				return
			}

			ctx := request.Context()
			target := backend.TargetUrl
			request.URL.Scheme = target.Scheme
			request.URL.Host = target.Host
			request.URL.Path = singleJoiningSlash(target.Path, request.URL.Path)

			//add custom headers
			host, _, _ := net.SplitHostPort(request.RemoteAddr)
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
			if startTime, ok := ctx.Value("request-timer").(time.Time); ok {
				reqLog.ProcessingTime = time.Since(startTime)
				reqLog.UpstreamTime = time.Since(ctx.Value("request-timer").(time.Time))
			}
			jsonLog, _ := json.Marshal(reqLog)
			log.Printf("Request Received: %s", jsonLog)
		},
	}

	proxy.Transport = &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		MaxConnsPerHost:     50,
		IdleConnTimeout:     90 * time.Second,
	}

	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		backend, ok := r.Context().Value("backend").(*lb.Backend)
		if ok {
			bkPool.UpdateBackendStatus(backend, false)
			log.Printf("proxy error (rid=%s, backend=%s): %v",
				r.Header.Get("X-Request-ID"),
				backend.Name,
				err)
		} else {
			log.Printf("proxy error (rid=%s): %v",
				r.Header.Get("X-Request-ID"),
				err)
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

	proxy.ModifyResponse = func(response *http.Response) error {
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
		startTime, ok := ctx.Value("request-timer").(time.Time)
		if !ok {
			return errors.New("request-timer not found in context")
		}
		var duration = time.Since(startTime)

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
		log.Printf("Response Received: %s", jsonLog)
		return nil
	}

	http.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		backend := bkPool.GetNextPeer()
		if backend == nil {
			http.Error(writer, "No healthy backends", http.StatusServiceUnavailable)
			return
		}

		// 2. Track active requests
		atomic.AddInt32(&backend.ActiveRequest, 1)
		defer atomic.AddInt32(&backend.ActiveRequest, -1)

		// 3. Store backend in context for Director to use
		ctx := context.WithValue(request.Context(), "backend", backend)
		ctx = context.WithValue(ctx, "request-timer", time.Now().UTC())
		request = request.WithContext(ctx)

		// 4. Proxy the request
		proxy.ServeHTTP(writer, request)
	})

	log.Println("Proxy server started on :8089")
	log.Fatal(http.ListenAndServe(":8089", nil))
}

type RequestLog struct {
	ID             string        `json:"id"`
	Method         string        `json:"method"`
	URL            string        `json:"url"`
	Headers        http.Header   `json:"headers"`
	Body           string        `json:"body"`
	RemoteAddr     string        `json:"remote_addr"`
	Timestamp      time.Time     `json:"timestamp"`
	Response       *ResponseLog  `json:"response,omitempty"`
	UpstreamTime   time.Duration `json:"upstream_time,omitempty"`
	ProcessingTime time.Duration `json:"processing_time,omitempty"`
}

type ResponseLog struct {
	Status     string        `json:"status"`
	StatusCode int           `json:"status_code"`
	Headers    http.Header   `json:"headers"`
	Body       string        `json:"body"`
	Timestamp  time.Time     `json:"timestamp"`
	Duration   time.Duration `json:"duration"`
}

// Helper function (from httputil internals)
func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
}

func mustParse(rawurl string) *url.URL {
	u, err := url.Parse(rawurl)
	if err != nil {
		panic(err)
	}
	return u
}
