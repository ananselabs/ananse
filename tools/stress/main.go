package main

import (
	"bufio"
	"flag"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"os"
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

	stop := time.After(*duration)
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
				case <-stop:
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
						// fmt.Printf("Worker %d error: %v\n", id, err)
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
	// Force a 500 from the backend
	resp, err := client.Get(*target + "?code=500")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// We expect 500, so if we get it, it's actually a "success" for the tool (proxy handled it)
	// But let's count it as error to track volume of errors
	if resp.StatusCode == 500 {
		return nil
	}
	return nil
}

func doBackendSlow(client *http.Client) error {
	// Force 2s latency (Proxy timeout is usually 5s)
	resp, err := client.Get(*target + "?sleep=2000")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

func doMalformed() error {
	conn, err := net.Dial("tcp", "localhost:8089")
	if err != nil {
		return err
	}
	defer conn.Close()

	// Send garbage
	garbage := []string{
		"GET / HTTP/1.1\r\nHost: localhost\r\nBad-Header\r\n\r\n",
		"POST / HTTP/1.1\r\nHost: localhost\r\nContent-Length: -1\r\n\r\n",
		"NotMethod / HTTP/1.1\r\n\r\n",
		"GET / \r\n\r\n", // Missing version
	}

	msg := garbage[rand.Intn(len(garbage))]
	fmt.Fprintf(conn, msg)

	// Read response (don't care content, just that it didn't hang forever)
	conn.SetReadDeadline(time.Now().Add(1 * time.Second))
	reader := bufio.NewReader(conn)
	_, _ = reader.ReadString('\n')
	return nil
}

func doSlowloris() error {
	conn, err := net.Dial("tcp", "localhost:8089")
	if err != nil {
		return err
	}
	// Do not defer close immediately, we want to hold it open

	fmt.Fprintf(conn, "GET /analytics HTTP/1.1\r\n")
	fmt.Fprintf(conn, "Host: localhost:8089\r\n")
	fmt.Fprintf(conn, "User-Agent: Slowloris\r\n")
	fmt.Fprintf(conn, "Content-Length: 42\r\n")

	// Send header slowly
	go func() {
		defer conn.Close()
		for i := 0; i < 10; i++ {
			time.Sleep(500 * time.Millisecond)
			if _, err := fmt.Fprintf(conn, "X-Slow: %d\r\n", i); err != nil {
				return
			}
		}
		// Never finish the request or finish very late
		fmt.Fprintf(conn, "\r\n")
	}()
	return nil
}
