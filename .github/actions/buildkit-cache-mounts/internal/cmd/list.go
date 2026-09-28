package cmd

import (
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/niledatabase/workflows/.github/actions/buildkit-cache-mounts/internal/buildkit"
)

func newListCommand(log *slog.Logger, rootOptions *rootCommandOptions) *cobra.Command {
	command := &cobra.Command{
		Use:   "list",
		Short: "List BuildKit cache mount IDs",
		Args:  cobra.NoArgs,
		PreRunE: func(*cobra.Command, []string) error {
			return rootOptions.prepare()
		},
		RunE: func(command *cobra.Command, _ []string) error {
			buildKit, err := buildkit.NewClient(command.Context(), rootOptions.builderName)
			if err != nil {
				return fmt.Errorf("create BuildKit client: %w", err)
			}
			defer buildKit.Close()

			cacheMounts, err := buildKit.DiscoverCacheMounts(command.Context(), log, rootOptions.dockerfilePath, rootOptions.targetPlatforms, rootOptions.targetStage, rootOptions.buildArgs)
			if err != nil {
				return err
			}
			for mountIndex, mount := range cacheMounts {
				if mountIndex > 0 && mount.ID() == cacheMounts[mountIndex-1].ID() {
					continue
				}
				if _, err := fmt.Fprintln(command.OutOrStdout(), mount.ID()); err != nil {
					return fmt.Errorf("write cache mount ID: %w", err)
				}
			}
			return nil
		},
	}
	return command
}
