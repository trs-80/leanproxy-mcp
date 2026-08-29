package cmd

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"

	"github.com/trs-80/leanproxy-mcp-bob/pkg/bouncer"
)

// upstreamErrorMessage is the fixed, client-facing text used whenever a
// request to an upstream server fails before a JSON-RPC response is
// received. The real error is never forwarded: transport errors routinely
// embed URLs with credentials, DSNs, or host details.
const upstreamErrorMessage = "upstream request failed"

// upstreamErrorResponse logs the real (redacted) error server-side together
// with a short opaque correlation id, and returns the generic client-facing
// message plus the id so operators can match a client report to the log line.
func upstreamErrorResponse(serverName, method string, err error) (msg, correlationID string) {
	correlationID = newCorrelationID()
	slog.Error("upstream request failed",
		"correlation_id", correlationID,
		"server", serverName,
		"method", method,
		"error", bouncer.RedactSecrets(err.Error()))
	return upstreamErrorMessage + " (ref " + correlationID + ")", correlationID
}

func newCorrelationID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000"
	}
	return hex.EncodeToString(b[:])
}
