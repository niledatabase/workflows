package main

import (
	"crypto/x509"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestFallbackRootsWithoutSystemCertificates(t *testing.T) {
	const childEnv = "BUILDKIT_CACHE_MOUNTS_TEST_FALLBACK_ROOTS"
	if os.Getenv(childEnv) == "1" {
		roots, err := x509.SystemCertPool()
		if err != nil {
			t.Fatal(err)
		}
		if roots == nil || roots.Equal(x509.NewCertPool()) {
			t.Fatal("no trusted roots available without system certificates")
		}
		return
	}

	// Use a fresh process because crypto/x509 caches system roots.
	directory := t.TempDir()
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestFallbackRootsWithoutSystemCertificates$")
	command.Env = append(os.Environ(),
		childEnv+"=1",
		"SSL_CERT_FILE="+filepath.Join(directory, "missing.pem"),
		"SSL_CERT_DIR="+directory,
		"GODEBUG=x509usefallbackroots=0",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("check fallback roots: %v\n%s", err, output)
	}
}
