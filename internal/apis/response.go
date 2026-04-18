package apis

import (
	"encoding/json"
	"net/http"
)

type ApiResponse struct {
	Data     any    `json:"data,omitempty"`
	Message  string `json:"message,omitempty"`
	Metadata string `json:"meta_data,omitempty"`
}

func WriteJson(w http.ResponseWriter, statusCode int, a ApiResponse) error {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(statusCode)
	return json.NewEncoder(w).Encode(a)
}
