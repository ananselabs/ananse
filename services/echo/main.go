package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("/echo", echoHandler)
	mux.HandleFunc("/health", healthHandler)
	port := os.Getenv("PORT")
	if port == "" {
		port = "4199"
	}

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
	}

	go func() {
		fmt.Printf("Server starting on :%s\n", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %s\n", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	log.Println("Shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal("Server forced to shutdown:", err)
	}

	log.Println("Server exiting")
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	response := map[string]string{
		"status":    "unhealthy",
		"timestamp": time.Now().Format(time.RFC3339),
	}

	json.NewEncoder(w).Encode(response)
}

func echoHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sleep := r.URL.Query().Get("sleep")
	var sleepMs int
	if sleep != "" {
		var err error
		// Convert to integer
		sleepMs, err = strconv.Atoi(sleep)
		fmt.Println("sleeping")
		if err != nil {
			http.Error(w, "Invalid sleep parameter", http.StatusBadRequest)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	data := map[string]interface{}{
		"method":      r.Method,
		"path":        r.URL,
		"query":       r.URL.Query(),
		"headers":     r.Header,
		"remote_addr": r.RemoteAddr,
		"sleep":       sleep,
	}

	// Use the value
	time.Sleep(time.Duration(sleepMs) * time.Millisecond)

	err := json.NewEncoder(w).Encode(data)
	if err != nil {
		return
	}
}
