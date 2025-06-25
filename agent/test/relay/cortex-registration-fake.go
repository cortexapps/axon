package main

// This is a fake implementation of the Cortex relay registration endpoint.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
)

// RegisterRequest represents the request body for the register endpoint
type RegisterRequest struct {
	Integration string `json:"integration"`
	Alias       string `json:"alias"`
	InstanceId  string `json:"instanceId"`
}

// RegisterResponse represents the response body for the register endpoint
type RegisterResponse struct {
	ServerUri string `json:"serverUri"`
	Token     string `json:"token"`
}

// handleRegister handles the /api/v1/relay/register route
func handleRegister(w http.ResponseWriter, r *http.Request) {

	// check token
	auth := r.Header.Get("Authorization")
	if auth == "" || !strings.HasPrefix(auth, "Bearer ") {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Ensure the request method is POST
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	fmt.Println("Received request /api/v1/relay/register")

	// Parse the JSON request body
	var req RegisterRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error parsing request body: %v", err), http.StatusBadRequest)
		return
	}

	// Process the request and create a response
	resp := RegisterResponse{
		ServerUri: os.Getenv("BROKER_SERVER_URL"),
		Token:     os.Getenv("TOKEN"),
	}

	// Set the response header to application/json
	w.Header().Set("Content-Type", "application/json")

	// Write the JSON response
	err = json.NewEncoder(w).Encode(resp)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error encoding response: %v", err), http.StatusInternalServerError)
		return
	}
}

func main() {

	if os.Getenv("BROKER_SERVER_URL") == "" {
		log.Fatal("BROKER_SERVER_URL environment variable not set")
	}

	// Register the /api/v1/relay/register route
	http.HandleFunc("/api/v1/relay/register", handleRegister)
	http.HandleFunc("/healthcheck", func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("Received request /healthcheck")
		w.WriteHeader(http.StatusOK)
	})

	// Start the HTTP server
	port := ":8080"
	if p := os.Getenv("PORT"); p != "" {
		port = ":" + p
	}
	log.Printf("Starting server on port %s", port)
	err := http.ListenAndServe(port, nil)
	if err != nil {
		log.Fatalf("Error starting server: %v", err)
	}
}
