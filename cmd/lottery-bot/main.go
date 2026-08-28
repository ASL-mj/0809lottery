package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"skyeapi/lottery-bot/internal/config"
	"skyeapi/lottery-bot/internal/web"
)

func main() {
	if len(os.Args) != 2 || os.Args[1] != "serve" {
		log.Fatal("usage: lottery-bot serve")
	}
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}
	if err := cfg.ValidateWeb(); err != nil {
		log.Fatalf("configuration error: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := web.NewServer(cfg).Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("web server stopped: %v", err)
	}
}
