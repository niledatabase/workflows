package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	cachearchive "github.com/niledatabase/workflows/.github/actions/buildkit-cache-mounts/internal/archive"
	"github.com/niledatabase/workflows/.github/actions/buildkit-cache-mounts/internal/frontend"
	actionscache "github.com/niledatabase/workflows/.github/actions/buildkit-cache-mounts/internal/github/actions/cache"
)

// executablePath is the path to which the Dockerfile of the frontend image copies the executable.
const executablePath = "/buildkit-cache-mounts"

// archiveArgs returns the command-line arguments that run transfer with the archive command of the frontend image.
func archiveArgs(transfer frontend.Transfer) []string {
	args := []string{
		executablePath, "archive", string(transfer.Operation),
		"--directory", transfer.Directory,
		"--key", transfer.Key,
		"--runtime-token-file", transfer.RuntimeTokenFile,
		"--results-url-file", transfer.ResultsURLFile,
	}
	for _, restoreKey := range transfer.RestoreKeys {
		args = append(args, "--restore-key", restoreKey)
	}
	if transfer.SkipNonEmpty {
		args = append(args, "--skip-non-empty")
	}
	return args
}

// newArchiveCommand returns the command that parses the arguments of archiveArgs and runs the transfer with run.
func newArchiveCommand(run func(context.Context, frontend.Transfer) error) *cobra.Command {
	command := &cobra.Command{
		Use:    "archive",
		Short:  "Transfer a cache mount between a directory and the GitHub Actions cache",
		Args:   cobra.NoArgs,
		Hidden: true,
	}
	command.AddCommand(
		newArchiveSubcommand(frontend.Restore, "Restore a directory from the GitHub Actions cache", run),
		newArchiveSubcommand(frontend.Save, "Save a directory to the GitHub Actions cache", run),
	)
	return command
}

func newArchiveSubcommand(operation frontend.Operation, short string, run func(context.Context, frontend.Transfer) error) *cobra.Command {
	transfer := frontend.Transfer{Operation: operation}
	command := &cobra.Command{
		Use:   string(operation),
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return run(command.Context(), transfer)
		},
	}
	flags := command.Flags()
	flags.StringVar(&transfer.Directory, "directory", "", "Directory of the cache mount")
	flags.StringVar(&transfer.Key, "key", "", "Cache key of the cache mount")
	flags.StringVar(&transfer.RuntimeTokenFile, "runtime-token-file", "", "File containing the GitHub Actions runtime token")
	flags.StringVar(&transfer.ResultsURLFile, "results-url-file", "", "File containing the GitHub Actions Results service URL")
	if operation == frontend.Restore {
		flags.StringArrayVar(&transfer.RestoreKeys, "restore-key", nil, "Key prefix for the restore when no exact key matches, in order of preference")
		flags.BoolVar(&transfer.SkipNonEmpty, "skip-non-empty", false, "Skip the restore if the directory has files")
	}
	for _, name := range []string{"directory", "key", "runtime-token-file", "results-url-file"} {
		command.MarkFlagRequired(name)
	}
	return command
}

// runTransfer runs transfer with the GitHub Actions cache.
func runTransfer(ctx context.Context, log *slog.Logger, transfer frontend.Transfer) error {
	runtimeToken, err := readSecretFile(transfer.RuntimeTokenFile)
	if err != nil {
		return err
	}
	resultsURL, err := readSecretFile(transfer.ResultsURLFile)
	if err != nil {
		return err
	}
	githubCache, err := actionscache.NewClient(resultsURL, runtimeToken)
	if err != nil {
		return fmt.Errorf("create GitHub cache client: %w", err)
	}
	log = log.With(slog.String("key", transfer.Key))
	if transfer.Operation == frontend.Restore {
		return restoreDirectory(ctx, log, githubCache, transfer)
	}
	return saveDirectory(ctx, log, githubCache, transfer)
}

func restoreDirectory(ctx context.Context, log *slog.Logger, githubCache *actionscache.Client, transfer frontend.Transfer) error {
	if transfer.SkipNonEmpty {
		empty, err := isEmptyDirectory(transfer.Directory)
		if err != nil {
			return err
		}
		if !empty {
			log.InfoContext(ctx, "skipped non-empty cache mount")
			return nil
		}
	}
	matchedKey, downloadURL, err := githubCache.LookupArchive(ctx, log, transfer.Key, transfer.RestoreKeys)
	if err != nil {
		return fmt.Errorf("lookup cache archive: %w", err)
	}
	if downloadURL == "" {
		log.InfoContext(ctx, "cache entry not found")
		return nil
	}
	if err := cachearchive.Download(ctx, transfer.Directory, downloadURL); err != nil {
		return fmt.Errorf("restore cache entry %q: %w", matchedKey, err)
	}
	log.InfoContext(ctx, "restored cache mount", slog.String("matched_key", matchedKey))
	return nil
}

func saveDirectory(ctx context.Context, log *slog.Logger, githubCache *actionscache.Client, transfer frontend.Transfer) error {
	empty, err := isEmptyDirectory(transfer.Directory)
	if err != nil {
		return err
	}
	if empty {
		log.InfoContext(ctx, "skipped empty cache mount")
		return nil
	}
	uploadURL, err := githubCache.ReserveArchive(ctx, log, transfer.Key)
	if err != nil {
		return fmt.Errorf("reserve cache archive: %w", err)
	}
	if uploadURL == "" {
		log.InfoContext(ctx, "cache entry exists or is reserved")
		return nil
	}
	compressedSize, err := cachearchive.Upload(ctx, transfer.Directory, uploadURL)
	if err != nil {
		return err
	}
	if err := githubCache.FinalizeArchive(ctx, log, transfer.Key, compressedSize); err != nil {
		return fmt.Errorf("finalize cache archive: %w", err)
	}
	log.InfoContext(ctx, "saved cache mount", slog.Int64("compressed_size_bytes", compressedSize))
	return nil
}

// readSecretFile returns the contents of the file path without a trailing newline.
func readSecretFile(path string) (string, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read secret file: %w", err)
	}
	secret := strings.TrimSuffix(string(contents), "\n")
	if secret == "" {
		return "", fmt.Errorf("secret file %s is empty", path)
	}
	return secret, nil
}

func isEmptyDirectory(directory string) (bool, error) {
	file, err := os.Open(directory)
	if err != nil {
		return false, fmt.Errorf("open cache mount: %w", err)
	}
	defer file.Close()
	if _, err := file.Readdirnames(1); err != nil {
		if errors.Is(err, io.EOF) {
			return true, nil
		}
		return false, fmt.Errorf("read cache mount: %w", err)
	}
	return false, nil
}
