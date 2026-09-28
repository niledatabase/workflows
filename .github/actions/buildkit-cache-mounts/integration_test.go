//go:build integration

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go/network"

	"github.com/niledatabase/workflows/.github/actions/buildkit-cache-mounts/internal/github/actions/cache/cachetest"
)

func TestBuildKitCacheRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	executable := executableUnderTest(ctx, t)
	builderName := fmt.Sprintf("cache-mount-integration-%d", os.Getpid())
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
	resultsURL := service.ResultsURL()

	removeBuildxBuilder(ctx, t, builderName)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		removeBuildxBuilder(cleanupCtx, t, builderName)
	})
	createBuildxBuilder(ctx, t, builderName, dockerNetwork.Name)

	contextDirectory := t.TempDir()
	populateDockerfilePath := filepath.Join(contextDirectory, "Dockerfile.populate")
	populateDockerfile := `
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
	if err := os.WriteFile(populateDockerfilePath, []byte(populateDockerfile), 0o600); err != nil {
		t.Fatal(err)
	}
	runBuildxBuild(ctx, t, builderName, contextDirectory, populateDockerfilePath)

	buildArgs := "BUILDKIT_CACHE_MOUNT_NS=namespace\nCACHE_ID=integration"
	commonArguments := []string{
		"--builder", builderName,
		"--file", populateDockerfilePath,
		"--build-args", buildArgs,
	}
	listedMounts := executeCommand(ctx, t, executable, append([]string{"list"}, commonArguments...)...)
	if got, want := listedMounts, fmt.Sprintf("namespace/integration-a-%[1]s\nnamespace/integration-b-%[1]s\n", runtime.GOARCH); got != want {
		t.Fatalf("listed cache mounts %q, want %q", got, want)
	}
	var rejectedRequests atomic.Int32
	rejectingService := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		rejectedRequests.Add(1)
		http.Error(response, "rejected", http.StatusBadRequest)
	}))
	defer rejectingService.Close()
	for _, command := range []string{"restore", "save"} {
		rejectedRequests.Store(0)
		arguments := append([]string{command}, commonArguments...)
		arguments = append(arguments,
			"--key", "integration-rejected",
			"--results-url", rejectingService.URL,
		)
		var exitErr *exec.ExitError
		if _, err := runCommand(ctx, t, executable, arguments...); !errors.As(err, &exitErr) {
			t.Fatalf("%s with a rejecting cache service returned %v, want a non-zero exit code", command, err)
		}
		if got := rejectedRequests.Load(); got != 2 {
			t.Fatalf("%s sent %d cache requests, want a request for each of the two mounts", command, got)
		}
	}
	saveArguments := append([]string{"save"}, commonArguments...)
	saveArguments = append(saveArguments,
		"--key", "integration-first",
		"--results-url", resultsURL,
	)
	executeCommand(ctx, t, executable, saveArguments...)
	executeCommand(ctx, t, executable, saveArguments...)

	removeBuildxBuilder(ctx, t, builderName)
	createBuildxBuilder(ctx, t, builderName, dockerNetwork.Name)
	restoreArguments := append([]string{"restore"}, commonArguments...)
	restoreArguments = append(restoreArguments,
		"--key", "integration-second",
		"--restore-keys", "integration-",
		"--results-url", resultsURL,
	)
	executeCommand(ctx, t, executable, restoreArguments...)

	verifyDockerfilePath := filepath.Join(contextDirectory, "Dockerfile.verify")
	verifyDockerfile := `
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
	if err := os.WriteFile(verifyDockerfilePath, []byte(verifyDockerfile), 0o600); err != nil {
		t.Fatal(err)
	}
	runBuildxBuild(ctx, t, builderName, contextDirectory, verifyDockerfilePath)
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

// executeCommand runs executable and returns its standard output. It fails the test if executable fails.
func executeCommand(ctx context.Context, t *testing.T, executable string, arguments ...string) string {
	t.Helper()
	output, err := runCommand(ctx, t, executable, arguments...)
	if err != nil {
		t.Fatalf("execute %q: %v", arguments, err)
	}
	return output
}

// runCommand runs executable with the runtime token of the test cache service, logs its standard error, and returns its standard output.
func runCommand(ctx context.Context, t *testing.T, executable string, arguments ...string) (string, error) {
	t.Helper()
	command := exec.CommandContext(ctx, executable, arguments...)
	command.Env = append(os.Environ(), "ACTIONS_RUNTIME_TOKEN="+cachetest.RuntimeToken)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	t.Logf("%s %q:\n%s", filepath.Base(executable), arguments, stderr.String())
	return stdout.String(), err
}

func createBuildxBuilder(ctx context.Context, t *testing.T, builderName, networkName string) {
	t.Helper()
	command := exec.CommandContext(
		ctx,
		"docker",
		"buildx",
		"create",
		"--name",
		builderName,
		"--driver",
		"docker-container",
		"--driver-opt",
		"image=docker.io/moby/buildkit:v0.34.1@sha256:99368f3e46177c6405b9740e4461d91cee004128daeecca78d4ceac50ec1da5c",
		"--driver-opt",
		"network="+networkName,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create builder: %v\n%s", err, output)
	}
	command = exec.CommandContext(ctx, "docker", "buildx", "inspect", "--builder", builderName, "--bootstrap")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("bootstrap builder: %v\n%s", err, output)
	}
}

func removeBuildxBuilder(ctx context.Context, t *testing.T, builderName string) {
	t.Helper()
	if err := exec.CommandContext(ctx, "docker", "buildx", "rm", "--force", builderName).Run(); err != nil {
		t.Logf("remove Buildx builder %q: %v", builderName, err)
	}
}

func runBuildxBuild(ctx context.Context, t *testing.T, builderName, contextDirectory, dockerfilePath string) {
	t.Helper()
	command := exec.CommandContext(
		ctx,
		"docker",
		"buildx",
		"build",
		"--builder",
		builderName,
		"--platform",
		"linux/"+runtime.GOARCH,
		"--file",
		dockerfilePath,
		"--build-arg",
		"BUILDKIT_CACHE_MOUNT_NS=namespace",
		"--output",
		"type=cacheonly",
		contextDirectory,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", filepath.Base(dockerfilePath), err, output)
	}
}
