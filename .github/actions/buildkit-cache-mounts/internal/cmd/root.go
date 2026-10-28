package cmd

import (
	"context"
	"log/slog"

	"github.com/moby/buildkit/frontend/gateway/grpcclient"
	"github.com/spf13/cobra"

	"github.com/niledatabase/workflows/.github/actions/buildkit-cache-mounts/internal/frontend"
)

// New returns the buildkit-cache-mounts root command.
// The command does not print errors, so the caller must report the error that Execute returns.
func New(log *slog.Logger) *cobra.Command {
	command := &cobra.Command{
		Use:           "buildkit-cache-mounts",
		Short:         "BuildKit frontend that keeps Dockerfile cache mounts in the GitHub Actions cache",
		SilenceErrors: true,
		SilenceUsage:  true,
		CompletionOptions: cobra.CompletionOptions{
			DisableDefaultCmd: true,
		},
	}
	command.AddCommand(
		newFrontendCommand(),
		newArchiveCommand(func(ctx context.Context, transfer frontend.Transfer) error {
			return runTransfer(ctx, log, transfer)
		}),
	)
	return command
}

func newFrontendCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "frontend",
		Short: "Run the BuildKit frontend",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return grpcclient.RunFromEnvironment(command.Context(), frontend.New(archiveArgs).Build)
		},
	}
}
