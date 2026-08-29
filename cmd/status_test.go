package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStatusCmd_Flags(t *testing.T) {
	boolFlags := []struct {
		name string
		flag string
	}{
		{"watch", "watch"},
		{"verbose", "verbose"},
		{"json", "json"},
		{"running", "running"},
	}

	for _, tt := range boolFlags {
		t.Run(tt.name, func(t *testing.T) {
			if err := statusCmd.Flags().Set(tt.flag, "true"); err != nil {
				t.Fatalf("set flag %s: %v", tt.flag, err)
			}

			got, err := statusCmd.Flags().GetBool(tt.flag)
			if err != nil {
				t.Fatalf("get flag %s: %v", tt.flag, err)
			}
			if !got {
				t.Errorf("flag %s = %v, want true", tt.flag, got)
			}
		})
	}

	stringFlags := []struct {
		name string
		flag string
		want string
	}{
		{"server", "server", "testserver"},
		{"config", "config", "/tmp/test.yaml"},
	}

	for _, tt := range stringFlags {
		t.Run(tt.name, func(t *testing.T) {
			if err := statusCmd.Flags().Set(tt.flag, tt.want); err != nil {
				t.Fatalf("set flag %s: %v", tt.flag, err)
			}

			got, err := statusCmd.Flags().GetString(tt.flag)
			if err != nil {
				t.Fatalf("get flag %s: %v", tt.flag, err)
			}
			if got != tt.want {
				t.Errorf("flag %s = %v, want %v", tt.flag, got, tt.want)
			}
		})
	}

	t.Run("interval", func(t *testing.T) {
		if err := statusCmd.Flags().Set("interval", "5s"); err != nil {
			t.Fatalf("set flag interval: %v", err)
		}

		got, err := statusCmd.Flags().GetDuration("interval")
		if err != nil {
			t.Fatalf("get flag interval: %v", err)
		}
		if got != 5*time.Second {
			t.Errorf("flag interval = %v, want 5s", got)
		}
	})
}

func TestStatusCmd_HelpRendersStatusHelpNotRootHelp(t *testing.T) {
	requireHelpFor(t, "status")
}

// TestStatusCmd_ReportsNoServersForAnEmptyConfig executes the command against
// a config it fully controls. Its predecessor asserted err == nil from
// statusCmd.Execute(), which never reached runStatus.
//
// --watch is deliberately never exercised here: runStatusWatch loops until
// interrupted (status.go:293), and a test that starts it would hang.
func TestStatusCmd_ReportsNoServersForAnEmptyConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "servers.yaml")
	if err := os.WriteFile(configPath, []byte("version: \"1.0\"\nservers: []\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("LEANPROXY_CONFIG", configPath)

	out := requireCLISucceeds(t, "status", "--config", configPath)

	if !strings.Contains(out, "No servers configured") {
		t.Errorf("status on an empty config should say so\ngot:\n%s", out)
	}
}

func TestStatusConfigPath(t *testing.T) {
	if statusConfigPath() == "" {
		t.Error("statusConfigPath should not return empty string when HOME is set")
	}
}

func TestGetRunningStatusList_NoRunningInstance(t *testing.T) {
	result := getRunningStatusList()
	_ = result
}
