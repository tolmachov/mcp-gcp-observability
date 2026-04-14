package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"

	"github.com/tolmachov/mcp-gcp-observability/internal"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Error("failed to load .env file", "err", err)
		os.Exit(1)
	}

	if err := internal.New(os.Stdin, os.Stdout, os.Stderr).Run(ctx, os.Args); err != nil {
		slog.Error("failed to run", "err", err)
		os.Exit(1)
	}
}
