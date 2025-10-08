package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "springg",
	Short: "Springg is a standalone vector database service",
	Long: `Springg is a lightweight, JWT-authenticated vector database service
that supports multiple named indexes with in-memory storage and disk persistence.`,
}

// Execute runs the root command
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	// Add subcommands
	rootCmd.AddCommand(serveCmd)
	rootCmd.AddCommand(generateJWTCmd)
	rootCmd.AddCommand(versionCmd)
}
