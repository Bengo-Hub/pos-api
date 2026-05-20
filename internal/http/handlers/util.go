package handlers

import (
	"encoding/json"
	"net/http"
)

// respondJSON writes the provided payload as a JSON response with the given status code.
// All handlers in this package should use this helper for consistency.
func respondJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

// jsonOK writes a 200 OK JSON response.
func jsonOK(w http.ResponseWriter, v interface{}) {
	respondJSON(w, http.StatusOK, v)
}

// jsonError writes a JSON error response with the given message and status code.
func jsonError(w http.ResponseWriter, msg string, code int) {
	respondJSON(w, code, map[string]string{"error": msg})
}
