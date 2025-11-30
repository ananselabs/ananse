package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"
)

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
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
		// Use the value
		time.Sleep(time.Duration(sleepMs) * time.Millisecond)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"service": "payments",
			"balance": 1000,
		})
	})

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("Ok"))
	})

	log.Println("Payment service listening on :5003")
	log.Fatal(http.ListenAndServe(":5003", nil))
}
