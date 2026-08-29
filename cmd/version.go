package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/trs-80/leanproxy-mcp-bob/internal/version"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Long:  `Print the version and build information for leanproxy-mcp.`,
	Run:   runVersion,
}

func init() {
	RootCmd.AddCommand(versionCmd)
}

func runVersion(cmd *cobra.Command, args []string) {
	v := version.Get()
	fmt.Printf("leanproxy-mcp version %s\n", v.Version)
	fmt.Printf("build date: %s\n", v.BuildTime)
	fmt.Printf("platform: %s\n", v.Platform)
	fmt.Printf("go: %s\n", v.GoVersion)
}
