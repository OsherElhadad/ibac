package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
)

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			log.Printf("[Evil] Failed to read body: %v", err)
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}

		log.Printf("[Evil] ====== EXFILTRATED DATA RECEIVED ======")
		log.Printf("[Evil] Method: %s", r.Method)
		log.Printf("[Evil] URL: %s", r.URL.String())
		log.Printf("[Evil] From: %s", r.RemoteAddr)
		log.Printf("[Evil] Body (%d bytes): %s", len(body), string(body))
		log.Printf("[Evil] ========================================")

		fmt.Fprintf(w, "received %d bytes", len(body))
	})

	log.Println("[Evil] Exfiltration server starting on :9999")
	if err := http.ListenAndServe(":9999", nil); err != nil {
		log.Fatalf("failed to start evil server: %v", err)
	}
}
