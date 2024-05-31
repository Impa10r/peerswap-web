package main

import (
	"log"
	"net/http"
	"peerswap-web/cmd/psweb/config"
	"strings"
	"time"
)

type responseWriter struct {
	http.ResponseWriter
	brokenPipe bool
}

func (rw *responseWriter) Write(p []byte) (int, error) {
	n, err := rw.ResponseWriter.Write(p)
	if err != nil {
		if strings.Contains(err.Error(), "stream closed") || strings.Contains(err.Error(), "broken pipe") {
			rw.brokenPipe = true
			log.Println("Detected broken pipe error")
		} else {
			log.Printf("Write error: %v", err)
		}
	}
	return n, err
}

// Middleware to retry on broken pipe
func retryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if config.Config.SecureConnection && r.TLS != nil {
			// Check client certificate
			cert := r.TLS.PeerCertificates
			if len(cert) == 0 {
				http.Error(w, "Client certificate not provided", http.StatusForbidden)
				return
			}
		}

		for i := 0; i < 3; i++ { // Retry up to 3 times
			rw := &responseWriter{ResponseWriter: w}
			next.ServeHTTP(rw, r)
			if !rw.brokenPipe {
				return
			}
			log.Println("Retrying due to broken pipe...")
			time.Sleep(1 * time.Second) // Wait before retrying
		}

		log.Println("Failed to handle request after 3 attempts due to broken pipe")
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
	})
}
