// Package netguard is the single place that decides whether leanproxy may
// send content to an inference endpoint.
//
// leanproxy sits between an agent and its tools, so everything it forwards is
// the user's working material: file paths, symbol names, search strings, tool
// arguments. Two features would hand that material to a *model* rather than to
// the tool it was meant for — manifest distillation (pkg/compactor) and
// embedding for the semantic cache. Both previously defaulted to OpenAI when
// left unconfigured, so enabling a feature was enough to ship a tool manifest
// off-box without ever naming a provider.
//
// Deployments under a data policy need "cannot happen", not "is switched off",
// so inference endpoints are now restricted to loopback: a local Ollama or
// llama.cpp is reachable, api.openai.com is not. This is deliberately narrow —
// it governs endpoints leanproxy sends *content to for inference*, and says
// nothing about upstream MCP servers, federation peers, or webhooks, which are
// infrastructure the operator configures and which legitimately live on other
// hosts.
package netguard

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// InferenceHostError reports an inference endpoint that is not loopback.
type InferenceHostError struct {
	Endpoint string
	Host     string
}

func (e *InferenceHostError) Error() string {
	return fmt.Sprintf(
		"inference endpoint %q resolves to non-loopback host %q; leanproxy only sends "+
			"content to a local inference endpoint (e.g. http://127.0.0.1:11434). "+
			"Point this at a model running on this machine",
		e.Endpoint, e.Host,
	)
}

// CheckInferenceEndpoint rejects any endpoint that would send content off this
// machine for inference. An empty endpoint is an error rather than a silent
// default: defaulting is exactly how the OpenAI endpoint used to get filled in.
//
// A hostname that resolves to a mix of loopback and routable addresses is
// rejected. DNS can change between this check and the request, so only a name
// that is unambiguously local passes.
func CheckInferenceEndpoint(endpoint string) error {
	raw := strings.TrimSpace(endpoint)
	if raw == "" {
		return fmt.Errorf("inference endpoint must be set explicitly (no default is applied)")
	}

	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("inference endpoint %q is not a valid URL: %w", endpoint, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("inference endpoint %q must use http or https, got %q", endpoint, u.Scheme)
	}

	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("inference endpoint %q has no host", endpoint)
	}

	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			return &InferenceHostError{Endpoint: endpoint, Host: host}
		}
		return nil
	}

	addrs, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("inference endpoint %q: cannot resolve host %q: %w", endpoint, host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("inference endpoint %q: host %q resolved to no addresses", endpoint, host)
	}
	for _, ip := range addrs {
		if !ip.IsLoopback() {
			return &InferenceHostError{Endpoint: endpoint, Host: ip.String()}
		}
	}
	return nil
}
