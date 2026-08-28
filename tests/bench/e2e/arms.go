package e2e

import (
	"bytes"
	"fmt"
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
	// with an unreachable or misconfigured upstream would still produce that
	// same valid, byte-identical payload, silently measuring an empty proxy
	// as if it fronted the full ballast. Confirm the wrapper can actually
	// reach the first configured server before trusting the measurement.
	if arm == ArmRouter && len(specs) > 0 {
		if err := verifyRouterReachable(c, specs[0]); err != nil {
			return nil, fmt.Errorf("router reachability check for arm %s: %w", arm, err)
		}
	}

	return payload, nil
}

// verifyRouterReachable calls the router's list_tools wrapper for the given
// server and confirms its first tool name appears in the result text.
func verifyRouterReachable(c *Client, first Spec) error {
	res, err := c.CallTool("list_tools", map[string]any{"server_name": first.Name})
	if err != nil {
		return fmt.Errorf("list_tools(%s): %w", first.Name, err)
	}
	want := first.Name + "_tool_0"
	if !bytes.Contains(res, []byte(want)) {
		return fmt.Errorf("list_tools(%s) result does not mention %q: %s", first.Name, want, res)
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
