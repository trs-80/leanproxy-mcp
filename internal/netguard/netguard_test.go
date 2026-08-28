package netguard

import (
	"strings"
	"testing"
)

func TestCheckInferenceEndpointAllowsLocal(t *testing.T) {
	for _, ep := range []string{
		"http://127.0.0.1:11434/v1/chat/completions",
		"http://localhost:11434/api/chat",
		"http://[::1]:8080/v1/chat/completions",
		"https://127.0.0.1:443/v1/chat/completions",
	} {
		if err := CheckInferenceEndpoint(ep); err != nil {
			t.Errorf("CheckInferenceEndpoint(%q) rejected a local endpoint: %v", ep, err)
		}
	}
}

// The endpoints this guard exists to stop. api.openai.com is the one the
// compactor used to fill in by itself when left unconfigured.
func TestCheckInferenceEndpointRejectsHosted(t *testing.T) {
	for _, ep := range []string{
		"https://api.openai.com/v1/chat/completions",
		"https://api.anthropic.com/v1/messages",
		"http://192.168.1.50:11434/v1/chat/completions",
		"https://8.8.8.8/v1/chat/completions",
	} {
		err := CheckInferenceEndpoint(ep)
		if err == nil {
			t.Errorf("CheckInferenceEndpoint(%q) allowed a non-loopback endpoint", ep)
			continue
		}
		if !strings.Contains(err.Error(), "non-loopback") {
			t.Errorf("CheckInferenceEndpoint(%q) rejected for the wrong reason: %v", ep, err)
		}
	}
}

// An empty endpoint must be an error, never a default. Silent defaulting is
// how the hosted endpoint got applied without anyone choosing it.
func TestCheckInferenceEndpointRejectsEmpty(t *testing.T) {
	for _, ep := range []string{"", "   "} {
		err := CheckInferenceEndpoint(ep)
		if err == nil {
			t.Fatalf("CheckInferenceEndpoint(%q) accepted an unset endpoint", ep)
		}
		if !strings.Contains(err.Error(), "explicitly") {
			t.Errorf("error should say the endpoint must be set explicitly, got: %v", err)
		}
	}
}

func TestCheckInferenceEndpointRejectsMalformed(t *testing.T) {
	for _, ep := range []string{
		"ftp://127.0.0.1/x",
		"file:///etc/passwd",
		"not a url at all",
		"http://",
	} {
		if err := CheckInferenceEndpoint(ep); err == nil {
			t.Errorf("CheckInferenceEndpoint(%q) accepted a malformed endpoint", ep)
		}
	}
}
