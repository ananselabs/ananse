package proxy

import (
	"net/http"
	"time"
)

type State int

const (
	Closed State = iota
	HalfOpen
	Open
)

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
