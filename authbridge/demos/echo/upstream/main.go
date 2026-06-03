// Trivial echo upstream for the credential placeholder-swap demo.
//
// Plain HTTP server with one route: GET /echo returns the
// Authorization header the request arrived with, as text/plain. In the
// demo this is the credential the authbridge sidecar produced AFTER
// resolving the agent's placeholder and exchanging it for an
// echo-upstream-audience token — so the body lets you see exactly what
// the upstream received, in contrast to the placeholder the agent held.
//
// Modeled on the IBAC demo's email-server skeleton, minus all the MCP /
// poisoned-email machinery. Reads PORT (default 8080).
package main

import (
	"log"
	"net/http"
	"os"
)

func handleEcho(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		auth = "(no Authorization header)"
	}
	log.Printf("[Upstream] /echo saw Authorization: %s", auth)
	w.Header().Set("Content-Type", "text/plain")
	if _, err := w.Write([]byte(auth)); err != nil {
		log.Printf("[Upstream] write response: %v", err)
	}
}

func main() {
	http.HandleFunc("/echo", handleEcho)
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	addr := ":" + port
	log.Printf("[Upstream] Echo upstream starting on %s/echo", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("failed to start upstream server: %v", err)
	}
}
