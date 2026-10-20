package main

import (
	"encoding/json"
	"log"
	"net/http"
)

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)
		w.Header().Set("Content-Type", "application/json")
		// Status 200 is the default
		if err := json.NewEncoder(w).Encode(map[string]string{"status": "ok"}); err != nil {
			log.Printf("[WRITE_ERROR] %s %s - %v", r.Method, r.URL.Path, err)
		}
	})

	addr := "127.0.0.1:8080"
	log.Printf("listening on %s", addr)       // Blocks forever and only returns on an error, such as the port already
	log.Fatal(http.ListenAndServe(addr, mux)) // Prints that error and exits
}
