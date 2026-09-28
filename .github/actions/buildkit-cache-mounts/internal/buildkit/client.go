package buildkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/docker/cli/cli/config"
	"github.com/docker/cli/cli/connhelper/commandconn"
	mobybuildkit "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	gateway "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth/authprovider"
	"github.com/moby/buildkit/session/secrets/secretsprovider"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/iohelper"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/tonistiigi/fsutil"

	cachearchive "github.com/niledatabase/workflows/.github/actions/buildkit-cache-mounts/internal/archive"
)

const (
	cacheMountPath          = "/cache"
	utilityExecutablePath   = "/buildkit-cache-mounts"
	utilityExecutableSource = "buildkit-cache-mounts-executable"
	cacheURLSecretID        = "buildkit-cache-url"
	cacheURLSecretPath      = "/run/secrets/buildkit-cache-url"
)

// Client discovers, imports, and exports the cache mounts of a Buildx builder.
// ImportCacheMount and ExportCacheMount run the current executable in a container on the native platform of the builder, so the executable must support that platform.
// Errors do not contain signed URLs.
type Client struct {
	client       *mobybuildkit.Client
	authProvider session.Attachable
}

type utilitySource struct {
	state       llb.State
	localMounts map[string]fsutil.FS
}

// NewClient starts the Buildx builder builderName if it does not run, and connects to it.
// The client gives the builder the registry credentials of the Docker configuration.
// The caller must call Close.
func NewClient(ctx context.Context, builderName string) (*Client, error) {
	inspectCommand := exec.CommandContext(ctx, "docker", "buildx", "inspect", "--builder", builderName, "--bootstrap")
	if output, err := inspectCommand.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("inspect Buildx builder %q: %w: %s", builderName, err, strings.TrimSpace(string(output)))
	}
	dockerConfig, err := config.Load(config.Dir())
	if err != nil {
		return nil, fmt.Errorf("load Docker configuration: %w", err)
	}
	authProvider := authprovider.NewDockerAuthProvider(authprovider.DockerAuthProviderConfig{AuthConfigProvider: authprovider.LoadAuthConfig(dockerConfig)})
	client, err := mobybuildkit.New(ctx, "", mobybuildkit.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return commandconn.New(ctx, "docker", "buildx", "dial-stdio", "--builder", builderName)
	}))
	if err != nil {
		return nil, fmt.Errorf("connect to Buildx builder %q: %w", builderName, err)
	}
	return &Client{client: client, authProvider: authProvider}, nil
}

// Close closes the connection to the builder.
func (b *Client) Close() error {
	return b.client.Close()
}

// DiscoverCacheMounts returns the cache mounts that a build of targetStage uses, for each platform in targetPlatforms or, without targetPlatforms, for the platform of the builder.
// It skips mounts with sharing=private and mounts whose input reads local files.
// It logs each skipped cache ID once: a private mount at info level, a mount whose input reads local files as a warning.
// It returns the mounts sorted by ID.
func (b *Client) DiscoverCacheMounts(
	ctx context.Context,
	log *slog.Logger,
	dockerfilePath string,
	targetPlatforms []ocispecs.Platform,
	targetStage string,
	buildArgs map[string]string,
) ([]Mount, error) {
	var cacheMounts []Mount
	err := b.build(ctx, mobybuildkit.SolveOpt{Session: []session.Attachable{b.authProvider}}, "buildkit-cache-mount-discovery", func(ctx context.Context, gatewayClient gateway.Client) error {
		buildPlatform, err := defaultWorkerPlatform(gatewayClient)
		if err != nil {
			return err
		}
		imageResolver := sourceresolver.NewImageMetaResolver(gatewayClient)
		cacheMounts, err = cacheMountsFromDockerfile(ctx, log, imageResolver, dockerfilePath, buildPlatform, targetPlatforms, targetStage, buildArgs)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("discover cache mounts: %w", err)
	}
	return cacheMounts, nil
}

// ImportCacheMount replaces the contents of mount in the builder with the archive at the signed URL downloadURL.
// If the extraction fails, mount is empty.
// It logs the log records of the cache mount process with log.
func (b *Client) ImportCacheMount(ctx context.Context, log *slog.Logger, mount Mount, downloadURL string) error {
	_, err := b.runCacheMountCommand(ctx, log, mount, downloadURL, "download")
	return err
}

// ExportCacheMount uploads an archive of mount in the builder to the signed URL uploadURL and returns the size of the archive in bytes.
// If mount is empty, it uploads nothing and returns 0.
// It logs the log records of the cache mount process with log.
func (b *Client) ExportCacheMount(ctx context.Context, log *slog.Logger, mount Mount, uploadURL string) (int64, error) {
	output, err := b.runCacheMountCommand(ctx, log, mount, uploadURL, "upload")
	if err != nil {
		return 0, err
	}
	result := cachearchive.UploadResult{}
	if err := json.Unmarshal(output, &result); err != nil {
		return 0, fmt.Errorf("decode cache archive upload result: %w", err)
	}
	return result.CompressedSize, nil
}

func (b *Client) runCacheMountCommand(ctx context.Context, log *slog.Logger, mount Mount, signedURL, operation string) ([]byte, error) {
	utility, err := prepareUtilitySource()
	if err != nil {
		return nil, err
	}
	secretProvider := secretsprovider.FromMap(map[string][]byte{cacheURLSecretID: []byte(signedURL)})
	attachables := []session.Attachable{b.authProvider, secretProvider}
	var stdout bytes.Buffer
	solveOpt := mobybuildkit.SolveOpt{Session: attachables, LocalMounts: utility.localMounts}
	err = b.build(ctx, solveOpt, "buildkit-cache-mounts", func(ctx context.Context, gatewayClient gateway.Client) (err error) {
		workerPlatform, err := defaultWorkerPlatform(gatewayClient)
		if err != nil {
			return err
		}
		utilityDefinition, err := utility.state.Marshal(ctx, llb.Platform(workerPlatform))
		if err != nil {
			return fmt.Errorf("marshal cache utility LLB: %w", err)
		}
		utilityResult, err := gatewayClient.Solve(ctx, gateway.SolveRequest{Definition: utilityDefinition.ToPB()})
		if err != nil {
			return fmt.Errorf("solve cache utility: %w", err)
		}
		gatewayMount := gateway.Mount{
			Dest:      cacheMountPath,
			MountType: pb.MountType_CACHE,
			CacheOpt: &pb.CacheOpt{
				ID:      mount.id,
				Sharing: mount.sharing,
			},
		}
		if mount.inputDefinition != nil {
			inputResult, err := gatewayClient.Solve(ctx, gateway.SolveRequest{Definition: mount.inputDefinition})
			if err != nil {
				return fmt.Errorf("solve cache mount input: %w", err)
			}
			gatewayMount.Ref = inputResult.Ref
			gatewayMount.Selector = mount.selector
		}
		container, err := gatewayClient.NewContainer(ctx, gateway.NewContainerRequest{
			Mounts: []gateway.Mount{
				{Dest: "/", MountType: pb.MountType_BIND, Ref: utilityResult.Ref, Readonly: true},
				gatewayMount,
				{
					Dest:      cacheURLSecretPath,
					MountType: pb.MountType_SECRET,
					SecretOpt: &pb.SecretOpt{ID: cacheURLSecretID, Mode: 0o400},
				},
			},
			Platform: pb.PlatformFromSpec(workerPlatform),
		})
		if err != nil {
			return fmt.Errorf("create cache mount container: %w", err)
		}
		defer func() {
			releaseErr := container.Release(ctx)
			// Release also returns the error of a failed process, which err already reports.
			if err == nil {
				err = releaseErr
			}
		}()

		// The archive commands log JSON records on standard error.
		stderrReader, stderrWriter := io.Pipe()
		var relog sync.WaitGroup
		relog.Go(func() {
			// If relogging stops before the output ends, the writes of the process fail instead of blocking.
			stderrReader.CloseWithError(relogProcessOutput(ctx, log, stderrReader))
		})
		defer func() {
			stderrWriter.Close()
			relog.Wait()
		}()

		process, err := container.Start(ctx, gateway.StartRequest{
			Args:   []string{utilityExecutablePath, "archive", operation, "--directory", cacheMountPath, "--url-file", cacheURLSecretPath},
			User:   "0",
			Stdout: &iohelper.NopWriteCloser{Writer: &stdout},
			Stderr: stderrWriter,
		})
		if err != nil {
			return fmt.Errorf("start cache mount process: %w", err)
		}
		if err := process.Wait(); err != nil {
			return fmt.Errorf("archive %s: %w", operation, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

// build runs buildFunc in a session of the builder.
// If buildFunc fails, build returns its error without the wrapping of the BuildKit client.
func (b *Client) build(ctx context.Context, solveOpt mobybuildkit.SolveOpt, product string, buildFunc func(context.Context, gateway.Client) error) error {
	var buildErr error
	_, err := b.client.Build(ctx, solveOpt, product, func(ctx context.Context, gatewayClient gateway.Client) (*gateway.Result, error) {
		buildErr = buildFunc(ctx, gatewayClient)
		return gateway.NewResult(), buildErr
	}, nil)
	if buildErr != nil {
		return buildErr
	}
	return err
}

// defaultWorkerPlatform returns the platform that the Dockerfile frontend uses as build platform and implicit target platform (Client.init in https://github.com/moby/buildkit/blob/v0.34.1/frontend/dockerui/config.go).
func defaultWorkerPlatform(gatewayClient gateway.Client) (ocispecs.Platform, error) {
	workers := gatewayClient.BuildOpts().Workers
	if len(workers) == 0 || len(workers[0].Platforms) == 0 {
		return ocispecs.Platform{}, errors.New("builder reports no worker platform")
	}
	return workers[0].Platforms[0], nil
}

func prepareUtilitySource() (utilitySource, error) {
	executablePath, err := os.Executable()
	if err != nil {
		return utilitySource{}, fmt.Errorf("locate buildkit-cache-mounts executable: %w", err)
	}
	executablePath, err = filepath.EvalSymlinks(executablePath)
	if err != nil {
		return utilitySource{}, fmt.Errorf("resolve buildkit-cache-mounts executable: %w", err)
	}
	executableFS, err := fsutil.NewFS(filepath.Dir(executablePath))
	if err != nil {
		return utilitySource{}, fmt.Errorf("create executable local source: %w", err)
	}

	executableName := filepath.Base(executablePath)
	executableSource := llb.Local(utilityExecutableSource, llb.FollowPaths([]string{executableName}))
	return utilitySource{
		state: llb.Scratch().
			File(llb.Copy(executableSource, executableName, utilityExecutablePath)),
		localMounts: map[string]fsutil.FS{
			utilityExecutableSource: executableFS,
		},
	}, nil
}
