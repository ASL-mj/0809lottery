package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"skyeapi/lottery-bot/internal/config"
	"skyeapi/lottery-bot/internal/web"
)

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: lottery-bot serve|migrate")
	}
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}
	switch os.Args[1] {
	case "serve":
		if err := cfg.ValidateServe(); err != nil {
			log.Fatalf("configuration error: %v", err)
		}
		serve(cfg)
	case "migrate":
		if err := runMigrate(cfg); err != nil {
			log.Fatalf("migration failed: %v", err)
		}
	default:
		log.Fatal("usage: lottery-bot serve|migrate")
	}
}

func serve(cfg config.Config) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := web.NewServer(cfg).Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("web server stopped: %v", err)
	}
}

func runMigrate(cfg config.Config) error {
	legacy, err := config.LoadLegacyAccounts(os.Getenv)
	if err != nil {
		return err
	}
	cfg.LegacyAccounts = legacy
	if err := cfg.ValidateMigrate(); err != nil {
		return err
	}
	return fmt.Errorf("migration requires the account registry, which is not installed yet")
}
