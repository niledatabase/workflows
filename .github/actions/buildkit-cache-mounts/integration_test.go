//go:build integration

package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/niledatabase/workflows/.github/actions/buildkit-cache-mounts/internal/github/actions/cache/cachetest"
)

const (
	buildkitImage           = "docker.io/moby/buildkit:v0.34.1@sha256:99368f3e46177c6405b9740e4461d91cee004128daeecca78d4ceac50ec1da5c"
	registryImage           = "docker.io/library/registry:3@sha256:d4032d4cc323c1731c707931ecada07a53076eac38e3b61ae57533f170f2c6c3"
	dockerfileFrontendImage = "docker.io/docker/dockerfile:1.28.0@sha256:bb22d9815c728170f72750f4e5b0d672e06176142e1d602c7e66c050100b7e5b"
)

const populateDockerfile = `
FROM ghcr.io/containerd/busybox:1.36@sha256:7b3ccabffc97de872a30dfd234fd972a66d247c8cfc69b0550f276481852627c
ARG CACHE_ID=integration
ARG TARGETARCH
RUN --mount=type=cache,id=${CACHE_ID}-a-${TARGETARCH},target=/cache-a,sharing=locked,mode=0770,uid=123,gid=456 \
    --mount=type=cache,id=${CACHE_ID}-b-${TARGETARCH},target=/cache-b,sharing=locked \
    sh -c 'mkdir -p /cache-a/directory && \
      printf cache-a > /cache-a/directory/value && \
      chmod 0750 /cache-a/directory && chmod 0640 /cache-a/directory/value && \
      ln /cache-a/directory/value /cache-a/directory/hardlink && \
      ln -s value /cache-a/directory/symlink && \
      mkfifo /cache-a/fifo && chmod 0620 /cache-a/fifo && \
      printf cache-b > /cache-b/value && \
      dd if=/dev/urandom of=/cache-b/random bs=1048576 count=9 status=none && \
      cd /cache-b && sha256sum random > random.sha256'
`

// verifyDockerfile checks the restored mounts of populateDockerfile, with another Dockerfile frontend in the syntax directive.
const verifyDockerfile = `# syntax=` + dockerfileFrontendImage + `
FROM ghcr.io/containerd/busybox:1.36@sha256:7b3ccabffc97de872a30dfd234fd972a66d247c8cfc69b0550f276481852627c
ARG CACHE_ID=integration
ARG TARGETARCH
RUN --mount=type=cache,id=${CACHE_ID}-a-${TARGETARCH},target=/cache-a,sharing=locked,mode=0770,uid=123,gid=456 \
    --mount=type=cache,id=${CACHE_ID}-b-${TARGETARCH},target=/cache-b,sharing=locked \
    test "$(cat /cache-a/directory/value)" = cache-a && \
    test "$(cat /cache-b/value)" = cache-b && \
    cd /cache-b && sha256sum -c random.sha256 && \
    test "$(readlink /cache-a/directory/symlink)" = value && \
    test "$(stat -c '%i' /cache-a/directory/value)" = "$(stat -c '%i' /cache-a/directory/hardlink)" && \
    test -p /cache-a/fifo && \
    test "$(stat -c '%a' /cache-a/directory/value)" = 640 && \
    test "$(stat -c '%a' /cache-a/fifo)" = 620 && \
    test "$(stat -c '%a:%u:%g' /cache-a)" = 770:123:456
`

func TestFrontendCacheRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	dockerNetwork, err := network.New(ctx)
	if err != nil {
		t.Fatalf("create Docker network: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := dockerNetwork.Remove(cleanupCtx); err != nil {
			t.Errorf("remove Docker network: %v", err)
		}
	})
	service := cachetest.New(t, dockerNetwork.Name)
	var rejectedRequests atomic.Int32
	rejectingService := cachetest.NewServer(t, dockerNetwork.Name, http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		rejectedRequests.Add(1)
		http.Error(response, "rejected", http.StatusBadRequest)
	}))
	registryHost := startRegistry(ctx, t, dockerNetwork)
	b := newBuilder(ctx, t, dockerNetwork.Name, registryHost)
	frontendImage := b.pushFrontendImage(ctx, t, registryHost)

	contextDirectory := t.TempDir()
	populatePath := filepath.Join(contextDirectory, "Dockerfile.populate")
	verifyPath := filepath.Join(contextDirectory, "Dockerfile.verify")
	for path, contents := range map[string]string{populatePath: populateDockerfile, verifyPath: verifyDockerfile} {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	b.build(ctx, t, contextDirectory, populatePath, frontendImage, rejectingService.URL)
	if got := rejectedRequests.Load(); got != 0 {
		t.Fatalf("build without key sent %d cache requests, want none", got)
	}

	// The build without key put files in both mounts. Cache mount a has an input, because it sets mode, uid, and gid.
	output := b.build(ctx, t, contextDirectory, populatePath, frontendImage, rejectingService.URL, "BUILDKIT_CACHE_MOUNTS_KEY=integration-rejected")
	if got := rejectedRequests.Load(); got != 3 {
		t.Fatalf("build with a rejecting cache service sent %d cache requests, want a restore request for mount a and a save request for each of the two mounts:\n%s", got, output)
	}
	for _, operation := range []string{"restore", "save"} {
		if want := "Failed to " + operation + " cache mount"; !strings.Contains(output, want) {
			t.Fatalf("build output has no warning %q:\n%s", want, output)
		}
	}

	output = b.build(ctx, t, contextDirectory, populatePath, frontendImage, service.ResultsURL(), "BUILDKIT_CACHE_MOUNTS_KEY=integration-first")
	if got := strings.Count(output, `msg="saved cache mount"`); got != 2 {
		t.Fatalf("build saved %d cache mounts, want 2:\n%s", got, output)
	}
	output = b.build(ctx, t, contextDirectory, populatePath, frontendImage, service.ResultsURL(), "BUILDKIT_CACHE_MOUNTS_KEY=integration-first")
	restored := strings.Count(output, `msg="restored cache mount"`)
	skipped := strings.Count(output, `msg="skipped non-empty cache mount"`)
	existing := strings.Count(output, `msg="cache entry exists or is reserved"`)
	if restored != 1 || skipped != 1 || existing != 2 {
		t.Fatalf("build restored %d cache mounts, skipped %d non-empty cache mounts, and found %d existing cache entries, want 1, 1, and 2:\n%s", restored, skipped, existing, output)
	}

	b.remove(ctx, t)
	b = newBuilder(ctx, t, dockerNetwork.Name, registryHost)
	output = b.build(ctx, t, contextDirectory, verifyPath, frontendImage, service.ResultsURL(),
		"BUILDKIT_CACHE_MOUNTS_KEY=integration-second",
		"BUILDKIT_CACHE_MOUNTS_RESTORE_KEYS=integration-",
	)
	if !strings.Contains(output, dockerfileFrontendImage) {
		t.Fatalf("build did not use the Dockerfile frontend of the syntax directive:\n%s", output)
	}
	if got := strings.Count(output, `msg="restored cache mount"`); got != 2 {
		t.Fatalf("build on a new builder restored %d cache mounts, want 2:\n%s", got, output)
	}
}

// startRegistry starts a registry in dockerNetwork and returns its host and port in dockerNetwork.
func startRegistry(ctx context.Context, t *testing.T, dockerNetwork *testcontainers.DockerNetwork) string {
	t.Helper()
	registry, err := testcontainers.Run(ctx, registryImage,
		network.WithNetwork(nil, dockerNetwork),
		testcontainers.WithExposedPorts("5000/tcp"),
		testcontainers.WithWaitStrategy(wait.ForListeningPort("5000/tcp")),
	)
	testcontainers.CleanupContainer(t, registry)
	if err != nil {
		t.Fatalf("start registry: %v", err)
	}
	registryIP, err := registry.ContainerIP(ctx)
	if err != nil {
		t.Fatalf("get registry container IP: %v", err)
	}
	return net.JoinHostPort(registryIP, "5000")
}

type builder struct {
	name string
}

// newBuilder creates a Buildx builder in the Docker network networkName that uses the registry at registryHost over HTTP, and removes it when the test ends.
func newBuilder(ctx context.Context, t *testing.T, networkName, registryHost string) builder {
	t.Helper()
	b := builder{name: fmt.Sprintf("buildkit-cache-mounts-integration-%d-%d", os.Getpid(), time.Now().UnixNano())}
	configPath := filepath.Join(t.TempDir(), "buildkitd.toml")
	if err := os.WriteFile(configPath, fmt.Appendf(nil, "[registry.%q]\n  http = true\n", registryHost), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		b.remove(cleanupCtx, t)
	})
	runDocker(ctx, t, nil, "buildx", "create",
		"--name", b.name,
		"--driver", "docker-container",
		"--driver-opt", "image="+buildkitImage,
		"--driver-opt", "network="+networkName,
		"--buildkitd-config", configPath,
	)
	runDocker(ctx, t, nil, "buildx", "inspect", "--builder", b.name, "--bootstrap")
	return b
}

func (b builder) remove(ctx context.Context, t *testing.T) {
	t.Helper()
	if output, err := exec.CommandContext(ctx, "docker", "buildx", "rm", "--force", b.name).CombinedOutput(); err != nil && !strings.Contains(string(output), "not found") {
		t.Logf("remove Buildx builder %q: %v\n%s", b.name, err, output)
	}
}

// pushFrontendImage builds the frontend image from Dockerfile, pushes it to the registry at registryHost, and returns its reference.
func (b builder) pushFrontendImage(ctx context.Context, t *testing.T, registryHost string) string {
	t.Helper()
	executable, err := os.ReadFile(executableUnderTest(ctx, t))
	if err != nil {
		t.Fatal(err)
	}
	contextDirectory := t.TempDir()
	if err := os.WriteFile(filepath.Join(contextDirectory, "buildkit-cache-mounts-linux-"+runtime.GOARCH), executable, 0o755); err != nil {
		t.Fatal(err)
	}
	image := registryHost + "/buildkit-cache-mounts:integration"
	runDocker(ctx, t, nil, "buildx", "build",
		"--builder", b.name,
		"--platform", "linux/"+runtime.GOARCH,
		"--provenance=false",
		"--file", "Dockerfile",
		"--output", "type=image,name="+image+",push=true",
		contextDirectory,
	)
	return image
}

// build builds dockerfilePath with frontendImage, the Results service at resultsURL, and buildArgs, and returns the build output.
func (b builder) build(ctx context.Context, t *testing.T, contextDirectory, dockerfilePath, frontendImage, resultsURL string, buildArgs ...string) string {
	t.Helper()
	arguments := []string{
		"buildx", "build",
		"--builder", b.name,
		"--platform", "linux/" + runtime.GOARCH,
		"--progress", "plain",
		"--file", dockerfilePath,
		"--build-arg", "BUILDKIT_SYNTAX=" + frontendImage,
		"--build-arg", "BUILDKIT_CACHE_MOUNT_NS=namespace",
		"--secret", "id=ACTIONS_RUNTIME_TOKEN,env=ACTIONS_RUNTIME_TOKEN",
		"--secret", "id=ACTIONS_RESULTS_URL,env=ACTIONS_RESULTS_URL",
		"--output", "type=cacheonly",
	}
	for _, buildArg := range buildArgs {
		arguments = append(arguments, "--build-arg", buildArg)
	}
	arguments = append(arguments, contextDirectory)
	return runDocker(ctx, t, []string{"ACTIONS_RUNTIME_TOKEN=" + cachetest.RuntimeToken, "ACTIONS_RESULTS_URL=" + resultsURL}, arguments...)
}

// runDocker runs the docker command with the additional environment variables env, and returns its combined output. It fails the test if the command fails.
func runDocker(ctx context.Context, t *testing.T, env []string, arguments ...string) string {
	t.Helper()
	command := exec.CommandContext(ctx, "docker", arguments...)
	command.Env = append(os.Environ(), env...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(arguments[:2], " "), err, output)
	}
	return string(output)
}

// executableUnderTest returns the executable in BUILDKIT_CACHE_MOUNTS_EXECUTABLE, or builds one.
func executableUnderTest(ctx context.Context, t *testing.T) string {
	t.Helper()
	if executable := os.Getenv("BUILDKIT_CACHE_MOUNTS_EXECUTABLE"); executable != "" {
		return executable
	}
	executable := filepath.Join(t.TempDir(), "buildkit-cache-mounts")
	command := exec.CommandContext(ctx, "go", "build", "-o", executable, ".")
	command.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build buildkit-cache-mounts executable: %v\n%s", err, output)
	}
	return executable
}
