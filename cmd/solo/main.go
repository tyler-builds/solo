// Command solo is the Solo desktop app: talk to an orchestrator that runs Claude worker threads across
// your repositories.
package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/tyler-builds/jamb/app"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/gui"
	"github.com/tyler-builds/solo/internal/orchestrator"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "solo:", err)
		os.Exit(1)
	}
}

func run() error {
	dataDir := core.DefaultDataDir()
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(dataDir, "solo.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()
	level := slog.LevelInfo
	if os.Getenv("SOLO_DEBUG") != "" {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(logFile, &slog.HandlerOptions{Level: level}))

	cmd, err := agent.FindCommand()
	if err != nil {
		return err
	}
	if os.Getenv("SOLO_DEBUG") != "" {
		cmd.Env = append(cmd.Env, "SOLO_AGENT_LOG_DIR="+filepath.Join(dataDir, "logs"))
	}
	c, err := core.New(core.Config{DataDir: dataDir, Agent: cmd, Log: log})
	if err != nil {
		return err
	}
	defer c.Close()
	o, err := orchestrator.New(c, dataDir, log)
	if err != nil {
		return err
	}
	g := gui.New(gui.Live{Core: c, O: o})
	defer g.Close()

	light, dark := gui.Themes()
	a := app.New().AppID("io.github.tyler_builds.solo")
	for _, f := range gui.Fonts() {
		a = a.Font("", f.Weight, f.Data)
	}
	a.Run(app.Window{
		Title:      "Solo",
		Width:      1440,
		Height:     900,
		MinWidth:   1200, // the sidebar, two panels side by side, and the rail
		MinHeight:  600,
		LightTheme: light,
		DarkTheme:  dark,
		Root:       g.Root,
	})
	return nil
}
