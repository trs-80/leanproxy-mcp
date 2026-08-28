package e2e

import (
	"bytes"
	"fmt"
	"strconv"
)

// Arm is one of the three client-visible configurations under comparison.
type Arm string

const (
	// ArmNative is no proxy at all: every server's full schema reaches the client.
	ArmNative Arm = "native"
	// ArmRouter is `server run --stdio` without --lazy-tools: the client sees
	// only list_tools/invoke_tool/search_tools and must discover before it can
	// invoke. This is actual lazy loading.
	ArmRouter Arm = "router"
	// ArmLazy is `server run --stdio --lazy-tools`: every upstream tool appears
	// by prefixed name with a compact stub schema. No discovery round trip is
	// needed, so this is schema compression rather than lazy loading.
	ArmLazy Arm = "lazy"
)

// AllArms returns the arms in ascending order of expected residency cost.
func AllArms() []Arm { return []Arm{ArmRouter, ArmLazy, ArmNative} }

// Capture returns the exact tools/list payload a client holds under arm.
// For the proxy arms it starts the real leanproxy binary over stdio; for the
// native arm it dials each upstream directly and concatenates, which is what a
// client with those servers configured would carry.
func Capture(arm Arm, leanproxyBin string, specs []Spec, dir string) ([]byte, error) {
	switch arm {
	case ArmNative:
		return captureNative(specs)
	case ArmRouter, ArmLazy:
		// handled below
	default:
		return nil, fmt.Errorf("unknown arm %q", arm)
	}

	cfg, err := WriteConfig(dir, specs)
	if err != nil {
		return nil, err
	}

	args := []string{"server", "run", "--stdio", "--config", cfg}
	if arm == ArmLazy {
		args = append(args, "--lazy-tools")
	}

	c, err := Dial(leanproxyBin, args...)
	if err != nil {
		return nil, fmt.Errorf("dial proxy for arm %s: %w", arm, err)
	}
	defer c.Close()

	if err := c.Initialize(); err != nil {
		return nil, fmt.Errorf("initialize arm %s: %w", arm, err)
	}

	payload, err := c.ToolsListRaw()
	if err != nil {
		return nil, err
	}

	// The router arm's client-visible tools/list is a fixed 3-tool wrapper
	// (list_tools/invoke_tool/search_tools) regardless of what is behind it —
	// TestCaptureRouterExposesWrapperTools guards that shape, but a capture
	// where one or more upstreams are unreachable or misconfigured would
	// still produce that same valid, byte-identical payload, silently
	// measuring a partially- or fully-empty proxy as if it fronted the full
	// ballast. Confirm the wrapper can actually reach EVERY configured
	// server before trusting the measurement — checking only the first spec
	// would still pass with e.g. specs[1] unreachable while ballast_tools
	// claims the count from all of them.
	if arm == ArmRouter {
		for _, spec := range specs {
			if err := verifyRouterReachable(c, spec); err != nil {
				return nil, fmt.Errorf("router reachability check for arm %s: %w", arm, err)
			}
		}
	}

	return payload, nil
}

// verifyRouterReachable calls the router's list_tools wrapper for spec and
// confirms it reports at least one cached tool for it. handleListTools
// (pkg/mcp/discovery.go) returns a successful response with a "<server>
// tools (<n>):" header when it can reach the server, and a different,
// still-successful-looking text ("Server '<server>' not found" or "No tools
// available on server '<server>'") when it cannot — neither is a JSON-RPC
// error, so ToolsListRaw alone can't tell them apart. This checks the
// reported count itself rather than any particular tool's name, so it makes
// no assumption about how the server names its tools (a generic Spec need
// not use mockmcp's "<prefix>_<n>" convention).
func verifyRouterReachable(c *Client, spec Spec) error {
	res, err := c.CallTool("list_tools", map[string]any{"server_name": spec.Name})
	if err != nil {
		return fmt.Errorf("list_tools(%s): %w", spec.Name, err)
	}

	marker := []byte(spec.Name + " tools (")
	idx := bytes.Index(res, marker)
	if idx < 0 {
		return fmt.Errorf("list_tools(%s) did not report a tool count (server unreachable or misconfigured): %s", spec.Name, res)
	}
	rest := res[idx+len(marker):]
	end := bytes.IndexByte(rest, ')')
	if end < 0 {
		return fmt.Errorf("list_tools(%s) malformed tool count: %s", spec.Name, res)
	}
	count, err := strconv.Atoi(string(rest[:end]))
	if err != nil {
		return fmt.Errorf("list_tools(%s) tool count not numeric: %s", spec.Name, res)
	}
	if count == 0 {
		return fmt.Errorf("list_tools(%s) reports 0 tools; router cannot reach this server", spec.Name)
	}
	return nil
}

// captureNative dials every upstream directly and joins their tools/list
// results. The join is a concatenation of result objects rather than a merged
// array because we are measuring bytes held in context, not building a working
// tool list.
func captureNative(specs []Spec) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, s := range specs {
		c, err := Dial(s.Command, s.Args...)
		if err != nil {
			return nil, fmt.Errorf("dial %s: %w", s.Name, err)
		}
		if err := c.Initialize(); err != nil {
			c.Close()
			return nil, fmt.Errorf("initialize %s: %w", s.Name, err)
		}
		raw, err := c.ToolsListRaw()
		c.Close()
		if err != nil {
			return nil, fmt.Errorf("tools/list %s: %w", s.Name, err)
		}
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(raw)
	}
	buf.WriteByte(']')
	return buf.Bytes(), nil
}
