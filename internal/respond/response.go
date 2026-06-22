package respond

import (
	"fmt"
	"net/http"
)

// WriteJSONError writes a JSON error response with the given status and message.
// The message is JSON-quoted so it may contain any characters.
func WriteJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error": %q}`, message)
}
