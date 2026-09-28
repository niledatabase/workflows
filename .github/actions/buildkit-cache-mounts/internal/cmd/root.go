package cmd

import (
	"encoding/csv"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/containerd/platforms"
	dockeropts "github.com/docker/cli/opts"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type rootCommandOptions struct {
	builderName     string
	dockerfilePath  string
	targetStage     string
	platformsInput  string
	targetPlatforms []ocispecs.Platform
	buildArgsInput  string
	buildArgs       map[string]string
}

// ErrReported means that the command failed and already logged the failure.
var ErrReported = errors.New("failure already reported")

// New returns the buildkit-cache-mounts root command.
// The command does not print errors, so the caller must report the error that Execute returns, unless the error wraps [ErrReported].
func New(log *slog.Logger) *cobra.Command {
	options := &rootCommandOptions{}
	command := &cobra.Command{
		Use:           "buildkit-cache-mounts",
		Short:         "Persist BuildKit cache mounts in GitHub Actions cache",
		SilenceErrors: true,
		SilenceUsage:  true,
		CompletionOptions: cobra.CompletionOptions{
			DisableDefaultCmd: true,
		},
	}
	options.addFlags(command.PersistentFlags())

	command.AddCommand(
		newArchiveCommand(),
		newListCommand(log, options),
		newCacheCommand(log, options, "restore", "Restore BuildKit cache mounts"),
		newCacheCommand(log, options, "save", "Save BuildKit cache mounts"),
	)
	return command
}

func (o *rootCommandOptions) addFlags(flags *pflag.FlagSet) {
	flags.StringVar(&o.builderName, "builder", "", "Buildx builder used by the image build")
	flags.StringVar(&o.dockerfilePath, "file", "Dockerfile", "Path to the Dockerfile to inspect")
	flags.StringVar(&o.targetStage, "target", "", "Dockerfile target stage to inspect")
	flags.StringVar(&o.platformsInput, "platforms", "", "Comma- or newline-separated target platforms of the image build (default: the platform of the builder)")
	flags.StringVar(&o.buildArgsInput, "build-args", "", "Newline-separated Dockerfile build arguments (NAME=VALUE or NAME)")
}

func (o *rootCommandOptions) prepare() error {
	if o.builderName == "" {
		return fmt.Errorf("builder is empty")
	}
	if o.dockerfilePath == "" {
		return fmt.Errorf("file is empty")
	}
	var err error
	o.targetPlatforms, err = parsePlatforms(o.platformsInput)
	if err != nil {
		return err
	}
	o.buildArgs, err = parseBuildArgs(o.buildArgsInput)
	return err
}

// parsePlatforms normalizes each platform like the Dockerfile frontend (parsePlatforms in https://github.com/moby/buildkit/blob/v0.34.1/frontend/dockerui/attr.go), so that the converted LLB matches the build.
func parsePlatforms(input string) ([]ocispecs.Platform, error) {
	var targetPlatforms []ocispecs.Platform
	for _, specifier := range strings.FieldsFunc(input, func(r rune) bool { return r == ',' || r == '\n' }) {
		specifier = strings.TrimSpace(specifier)
		if specifier == "" {
			continue
		}
		platform, err := platforms.Parse(specifier)
		if err != nil {
			return nil, fmt.Errorf("parse platform: %w", err)
		}
		targetPlatforms = append(targetPlatforms, platforms.Normalize(platform))
	}
	return targetPlatforms, nil
}

func parseBuildArgs(input string) (map[string]string, error) {
	// Same parsing as the build-args input of docker/build-push-action (https://github.com/docker/build-push-action/blob/v7.4.0/src/context.ts).
	// That input uses Util.getInputList with ignoreComma (https://github.com/docker/actions-toolkit/blob/v0.100.0/src/util.ts).
	csvReader := csv.NewReader(strings.NewReader(input))
	csvReader.FieldsPerRecord = -1
	csvReader.LazyQuotes = true
	records, err := csvReader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parse build arguments: %w", err)
	}

	validatedArguments := make([]string, 0, len(records))
	for _, record := range records {
		argument := strings.TrimSpace(strings.Join(record, ","))
		if argument == "" {
			continue
		}
		validatedArgument, err := dockeropts.ValidateEnv(argument)
		if err != nil {
			return nil, fmt.Errorf("validate build argument: %w", err)
		}
		if strings.Contains(validatedArgument, "=") {
			validatedArguments = append(validatedArguments, validatedArgument)
		}
	}
	return dockeropts.ConvertKVStringsToMap(validatedArguments), nil
}
