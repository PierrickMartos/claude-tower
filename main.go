package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"

	"claude-tower/internal/cmuxevents"
	"claude-tower/internal/registry"
	"claude-tower/internal/summarizer"
	"claude-tower/internal/transcript"
	"claude-tower/internal/ui"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	reg := registry.New()
	sum := summarizer.New()

	discoveries, _ := transcript.DiscoverActive()
	for _, d := range discoveries {
		if s := reg.BootstrapSession(d.SessionID, d.Cwd, d.LastTool, d.LastEvent); s != nil {
			sid, cwd := s.ID, s.Cwd
			sum.Request(sid, cwd, func(r summarizer.Result) {
				if r.Err == nil {
					reg.SetSummary(r.SessionID, r.Summary)
				}
			})
		}
	}

	evCh, err := cmuxevents.Subscribe(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "subscribe:", err)
		os.Exit(1)
	}

	model := ui.NewModel(reg, sum)
	p := tea.NewProgram(model, tea.WithAltScreen())

	go func() {
		for ev := range evCh {
			p.Send(ui.EventMsg{Event: ev})
		}
	}()

	if _, err := p.Run(); err != nil {
		log.Fatal(err)
	}
}
