package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"syscall"

	"skyeapi/lottery-bot/internal/config"
	"skyeapi/lottery-bot/internal/secret"
	"skyeapi/lottery-bot/internal/state"
	"skyeapi/lottery-bot/internal/version"
	"skyeapi/lottery-bot/internal/web"
)

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: lottery-bot serve|migrate|version")
	}
	if os.Args[1] == "version" {
		fmt.Println(version.Version)
		return
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
		log.Fatal("usage: lottery-bot serve|migrate|version")
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
	vault, err := secret.NewFileVault(cfg.VaultPath, cfg.VaultKey)
	if err != nil {
		return err
	}
	accounts := make([]state.LegacyAccount, 0, len(legacy))
	for id, account := range legacy {
		accounts = append(accounts, state.LegacyAccount{
			ID:        id,
			Label:     account.Label,
			LoginName: account.Username,
			Password:  account.Password,
		})
	}
	sort.Slice(accounts, func(left, right int) bool { return accounts[left].ID < accounts[right].ID })
	result, err := state.MigrateV3(context.Background(), cfg.StatePath, vault, accounts)
	if err != nil {
		return err
	}
	log.Printf("migration finished: %s", result.Message)
	return nil
}
