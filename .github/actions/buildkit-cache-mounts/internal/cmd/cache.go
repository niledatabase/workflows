package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/niledatabase/workflows/.github/actions/buildkit-cache-mounts/internal/buildkit"
	actionscache "github.com/niledatabase/workflows/.github/actions/buildkit-cache-mounts/internal/github/actions/cache"
)

const mountConcurrency = 4

func newCacheCommand(log *slog.Logger, rootOptions *rootCommandOptions, operation, short string) *cobra.Command {
	var key, resultsURL, restoreKeysInput string
	command := &cobra.Command{
		Use:   operation,
		Short: short,
		Args:  cobra.NoArgs,
		PreRunE: func(*cobra.Command, []string) error {
			return rootOptions.prepare()
		},
		RunE: func(command *cobra.Command, _ []string) error {
			ctx := command.Context()
			log := log.With(slog.String("operation", operation))
			restoreKeys := parseRestoreKeys(restoreKeysInput)
			if err := validateCacheKeyInputs(key, restoreKeys); err != nil {
				return err
			}
			runtimeToken := os.Getenv("ACTIONS_RUNTIME_TOKEN")
			if runtimeToken == "" {
				return fmt.Errorf("ACTIONS_RUNTIME_TOKEN is empty")
			}
			githubCache, err := actionscache.NewClient(resultsURL, runtimeToken)
			if err != nil {
				return fmt.Errorf("create GitHub cache: %w", err)
			}
			buildKit, err := buildkit.NewClient(ctx, rootOptions.builderName)
			if err != nil {
				return fmt.Errorf("create BuildKit client: %w", err)
			}
			defer buildKit.Close()

			cacheMounts, err := buildKit.DiscoverCacheMounts(ctx, log, rootOptions.dockerfilePath, rootOptions.targetPlatforms, rootOptions.targetStage, rootOptions.buildArgs)
			if err != nil {
				return err
			}

			var transferredCount, skippedCount atomic.Int64
			var group errgroup.Group
			group.SetLimit(mountConcurrency)
			for _, mount := range cacheMounts {
				group.Go(func() error {
					key, mountRestoreKeys := cacheMountKeys(mount.IdentityHash(), key, restoreKeys)
					log := log.With(
						slog.String("id", mount.ID()),
						slog.String("key", key),
					)
					var transferred bool
					var err error
					if operation == "restore" {
						transferred, err = restoreCacheMount(ctx, log, githubCache, buildKit, mount, key, mountRestoreKeys)
					} else {
						transferred, err = saveCacheMount(ctx, log, githubCache, buildKit, mount, key)
					}
					switch {
					case ctx.Err() != nil:
						// The command returns the cancellation cause instead of a failure for each mount.
						return context.Cause(ctx)
					case err != nil:
						log.ErrorContext(ctx, "cache mount transfer failed", slog.Any("error", err))
						return ErrReported
					case transferred:
						transferredCount.Add(1)
					default:
						skippedCount.Add(1)
					}
					return nil
				})
			}
			err = group.Wait()
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
			log.InfoContext(ctx, "finished cache mount transfers",
				slog.Int64("transferred", transferredCount.Load()),
				slog.Int64("skipped", skippedCount.Load()),
				slog.Int64("failed", int64(len(cacheMounts))-transferredCount.Load()-skippedCount.Load()),
			)
			return err
		},
	}

	command.Flags().StringVar(&key, "key", "", "Cache key used to restore and save each discovered mount")
	command.Flags().StringVar(&resultsURL, "results-url", "", "GitHub Actions Results service URL")
	if operation == "restore" {
		command.Flags().StringVar(&restoreKeysInput, "restore-keys", "", "Ordered newline-separated prefixes used when no exact key matches")
	}
	command.MarkFlagRequired("results-url")
	return command
}

func restoreCacheMount(ctx context.Context, log *slog.Logger, githubCache *actionscache.Client, buildKit *buildkit.Client, mount buildkit.Mount, key string, restoreKeys []string) (bool, error) {
	matchedKey, downloadURL, err := githubCache.LookupArchive(ctx, log, key, restoreKeys)
	if err != nil {
		return false, fmt.Errorf("lookup cache archive: %w", err)
	}
	if downloadURL == "" {
		log.InfoContext(ctx, "cache entry not found")
		return false, nil
	}
	if err := buildKit.ImportCacheMount(ctx, log, mount, downloadURL); err != nil {
		return false, fmt.Errorf("import cache mount from cache entry %q: %w", matchedKey, err)
	}
	log.InfoContext(ctx, "restored cache mount", slog.String("matched_key", matchedKey))
	return true, nil
}

func saveCacheMount(ctx context.Context, log *slog.Logger, githubCache *actionscache.Client, buildKit *buildkit.Client, mount buildkit.Mount, key string) (bool, error) {
	uploadURL, err := githubCache.ReserveArchive(ctx, log, key)
	if err != nil {
		return false, fmt.Errorf("reserve cache archive: %w", err)
	}
	if uploadURL == "" {
		log.InfoContext(ctx, "cache entry exists or is reserved")
		return false, nil
	}
	compressedSize, err := buildKit.ExportCacheMount(ctx, log, mount, uploadURL)
	if err != nil {
		return false, fmt.Errorf("export cache mount: %w", err)
	}
	if compressedSize == 0 {
		log.InfoContext(ctx, "skipped empty cache mount")
		return false, nil
	}
	if err := githubCache.FinalizeArchive(ctx, log, key, compressedSize); err != nil {
		return false, fmt.Errorf("finalize cache archive: %w", err)
	}
	log.InfoContext(ctx, "saved cache mount", slog.Int64("compressed_size_bytes", compressedSize))
	return true, nil
}

func parseRestoreKeys(input string) []string {
	var restoreKeys []string
	for _, line := range strings.Split(input, "\n") {
		if key := strings.TrimSpace(line); key != "" {
			restoreKeys = append(restoreKeys, key)
		}
	}
	return restoreKeys
}
