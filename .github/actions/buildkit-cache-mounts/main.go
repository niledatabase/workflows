package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	_ "golang.org/x/crypto/x509roots/fallback" // Supply trusted roots in the scratch image.

	"github.com/niledatabase/workflows/.github/actions/buildkit-cache-mounts/internal/cmd"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := cmd.New(log).ExecuteContext(ctx)
	stop()
	if err != nil {
		log.ErrorContext(ctx, "command failed", slog.Any("error", err))
		os.Exit(1)
	}
}
