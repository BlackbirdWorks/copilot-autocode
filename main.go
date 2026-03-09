// copilot-autocode is a terminal UI application that acts as a headless
// "Copilot Orchestrator".  It manages a queue of GitHub issues, feeds them to
// the native GitHub Copilot coding agent, and babysits the resulting pull
// requests through CI feedback and merging.
//
// Usage:
//
//	GITHUB_TOKEN=<pat> copilot-autocode [--config config.yaml]
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/BlackbirdWorks/copilot-autocode/config"
	"github.com/BlackbirdWorks/copilot-autocode/ghclient"
	"github.com/BlackbirdWorks/copilot-autocode/poller"
	"github.com/BlackbirdWorks/copilot-autocode/tui"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config.yaml")
	flag.Parse()

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		log.Fatal("GITHUB_TOKEN environment variable is required")
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	gh := ghclient.New(token, cfg)
	ctx, cancel := context.WithCancel(context.Background())

	// Create and start the background poller.
	p := poller.New(cfg, gh, token)
	p.Start(ctx)

	// Create the Bubble Tea model.
	model := tui.New(cfg.GitHubOwner, cfg.GitHubRepo, cfg.PollIntervalSeconds)

	prog := tea.NewProgram(
		model,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)

	// Bridge poller events → Bubble Tea messages in a goroutine.
	go func() {
		for evt := range p.Events {
			prog.Send(tui.PollEvent{Event: evt})
		}
	}()

	// Handle OS signals for graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
		prog.Quit()
	}()

	if _, err := prog.Run(); err != nil {
		cancel()
		fmt.Fprintf(os.Stderr, "error running TUI: %v\n", err)
		os.Exit(1)
	}
	cancel()
}
