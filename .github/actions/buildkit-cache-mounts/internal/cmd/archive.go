package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	cachearchive "github.com/niledatabase/workflows/.github/actions/buildkit-cache-mounts/internal/archive"
)

const maximumSignedURLSize = 64 << 10

type archiveCommandOptions struct {
	directory string
	urlFile   string
}

func newArchiveCommand() *cobra.Command {
	command := &cobra.Command{
		Use:    "archive",
		Short:  "Transfer cache mount archives",
		Args:   cobra.NoArgs,
		Hidden: true,
	}
	command.AddCommand(
		newArchiveSubcommand("upload", "Archive and upload a directory", uploadArchive),
		newArchiveSubcommand("download", "Download and extract an archive into a directory", downloadArchive),
	)
	return command
}

// newArchiveSubcommand returns an archive command that runs run.
// The command logs JSON records on standard error, because runCacheMountCommand logs these records again with the context of the cache mount.
func newArchiveSubcommand(use, short string, run func(ctx context.Context, options *archiveCommandOptions, output io.Writer) error) *cobra.Command {
	options := &archiveCommandOptions{}
	command := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			ctx := command.Context()
			log := slog.New(slog.NewJSONHandler(command.ErrOrStderr(), nil))
			err := options.validate()
			if err == nil {
				err = run(ctx, options, command.OutOrStdout())
			}
			if err != nil {
				log.ErrorContext(ctx, "archive command failed", slog.Any("error", err))
				return ErrReported
			}
			return nil
		},
	}
	options.addFlags(command.Flags())
	return command
}

func (o *archiveCommandOptions) addFlags(flags *pflag.FlagSet) {
	flags.StringVar(&o.directory, "directory", "", "Directory to archive or restore")
	flags.StringVar(&o.urlFile, "url-file", "", "File containing the signed Azure Blob URL")
}

func (o *archiveCommandOptions) validate() error {
	if o.directory == "" {
		return fmt.Errorf("directory is empty")
	}
	if o.urlFile == "" {
		return fmt.Errorf("url-file is empty")
	}
	return nil
}

func uploadArchive(ctx context.Context, options *archiveCommandOptions, output io.Writer) error {
	uploadURL, err := readSignedURL(options.urlFile)
	if err != nil {
		return err
	}
	size, err := cachearchive.Upload(ctx, options.directory, uploadURL)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(output).Encode(cachearchive.UploadResult{CompressedSize: size}); err != nil {
		return fmt.Errorf("write archive upload result: %w", err)
	}
	return nil
}

func downloadArchive(ctx context.Context, options *archiveCommandOptions, _ io.Writer) error {
	downloadURL, err := readSignedURL(options.urlFile)
	if err != nil {
		return err
	}
	return cachearchive.Download(ctx, options.directory, downloadURL)
}

func readSignedURL(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("open signed URL file: %w", err)
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, maximumSignedURLSize+1))
	if err != nil {
		return "", fmt.Errorf("read signed URL file: %w", err)
	}
	if len(contents) > maximumSignedURLSize {
		return "", fmt.Errorf("signed URL exceeds %d bytes", maximumSignedURLSize)
	}
	contents = bytes.TrimSuffix(contents, []byte("\n"))
	if len(contents) == 0 {
		return "", fmt.Errorf("signed URL file is empty")
	}
	if bytes.ContainsAny(contents, "\r\n\x00") {
		return "", fmt.Errorf("signed URL contains an invalid character")
	}
	return string(contents), nil
}
