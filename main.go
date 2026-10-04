package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	config, err := LoadConfig()
	if err != nil {
		log.Fatalf("configuration: %v", err)
	}
	store, err := NewStore(config.DBPath)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer store.Close()

	smtpServer := NewSMTPServer(config, store)
	if err := smtpServer.Start(); err != nil {
		log.Fatalf("SMTP listen: %v", err)
	}
	httpServer := NewHTTPServer(config, store)
	httpErr := make(chan error, 1)
	go func() { httpErr <- httpServer.ListenAndServe() }()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	select {
	case <-ctx.Done():
	case err := <-httpErr:
		if err != nil {
			log.Fatalf("HTTP listen: %v", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP shutdown: %v", err)
	}
	if err := smtpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("SMTP shutdown: %v", err)
	}
}
