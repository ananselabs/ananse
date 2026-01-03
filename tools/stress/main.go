package main

import (
	"bufio"
	"context" // Import context
	"flag"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"sync"
	"time"
)

var (
	target   = flag.String("target", "http://localhost:8089/analytics", "Target URL")
	workers  = flag.Int("workers", 10, "Number of concurrent workers")
	duration = flag.Duration("duration", 10*time.Second, "Test duration")
	mode     = flag.String("mode", "mixed", "Mode: normal, mixed, slowloris, malformed")
)

func main() {
	flag.Parse()
	fmt.Printf("Starting Stress Tool - Mode: %s, Workers: %d, Duration: %s\n", *mode, *workers, *duration)

	// FIX 1: Use Context for timeout instead of time.After
	// This ensures the cancellation signal is broadcast to ALL workers
	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()

	var wg sync.WaitGroup

	// Stats
	var (
		successCount int64
		failCount    int64
		mu           sync.Mutex
	)

	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			client := &http.Client{
				Timeout: 5 * time.Second,
			}

			for {
				select {
				// FIX 2: Listen to ctx.Done()
				// When context expires, this channel is CLOSED, unblocking everyone immediately
				case <-ctx.Done():
					return
				default:
					// Action based on mode
					var err error
					switch *mode {
					case "mixed":
						r := rand.Intn(100)
						if r < 80 {
							err = doNormal(client)
						} else if r < 90 {
							err = doBackendError(client)
						} else {
							err = doBackendSlow(client)
						}
					case "malformed":
						err = doMalformed()
					case "slowloris":
						err = doSlowloris()
					default:
						err = doNormal(client)
					}

					mu.Lock()
					if err == nil {
						successCount++
					} else {
						failCount++
					}
					mu.Unlock()

					// Slight jitter
					time.Sleep(time.Duration(rand.Intn(50)) * time.Millisecond)
				}
			}
		}(i)
	}

	wg.Wait()
	fmt.Printf("--- REPORT ---\nSuccess: %d\nFailures: %d\n", successCount, failCount)
}

// ... (Rest of your functions doNormal, doBackendError, etc. remain exactly the same) ...

func doNormal(client *http.Client) error {
	resp, err := client.Get(*target)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("server error: %d", resp.StatusCode)
	}
	return nil
}

func doBackendError(client *http.Client) error {
	resp, err := client.Get(*target + "?code=500")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 500 {
		return nil
	}
	return nil
}

func doBackendSlow(client *http.Client) error {
	resp, err := client.Get(*target + "?sleep=2000")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

func doMalformed() error {
	conn, err := net.DialTimeout("tcp", "localhost:8089", 2*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()

	garbage := []string{
		"GET / HTTP/1.1\r\nHost: localhost\r\nBad-Header\r\n\r\n",
		"POST / HTTP/1.1\r\nHost: localhost\r\nContent-Length: -1\r\n\r\n",
		"NotMethod / HTTP/1.1\r\n\r\n",
		"GET / \r\n\r\n",
	}

	msg := garbage[rand.Intn(len(garbage))]
	fmt.Fprintf(conn, msg)

	conn.SetReadDeadline(time.Now().Add(1 * time.Second))
	reader := bufio.NewReader(conn)
	_, _ = reader.ReadString('\n')
	return nil
}

func doSlowloris() error {
	conn, err := net.DialTimeout("tcp", "localhost:8089", 2*time.Second)
	if err != nil {
		return err
	}

	fmt.Fprintf(conn, "GET /analytics HTTP/1.1\r\n")
	fmt.Fprintf(conn, "Host: localhost:8089\r\n")
	fmt.Fprintf(conn, "User-Agent: Slowloris\r\n")
	fmt.Fprintf(conn, "Content-Length: 42\r\n")

	go func() {
		defer conn.Close()
		for i := 0; i < 10; i++ {
			time.Sleep(500 * time.Millisecond)
			if _, err := fmt.Fprintf(conn, "X-Slow: %d\r\n", i); err != nil {
				return
			}
		}
		fmt.Fprintf(conn, "\r\n")
	}()
	return nil
}
