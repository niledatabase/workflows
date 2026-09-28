package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	_ "golang.org/x/crypto/x509roots/fallback" // Supply trusted roots in the scratch helper container.

	"github.com/niledatabase/workflows/.github/actions/buildkit-cache-mounts/internal/cmd"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := cmd.New(log).ExecuteContext(ctx)
	stop()
	if err != nil {
		if !errors.Is(err, cmd.ErrReported) {
			log.ErrorContext(ctx, "command failed", slog.Any("error", err))
		}
		os.Exit(1)
	}
}
