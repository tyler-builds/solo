// Command solo is the Solo desktop app: talk to an orchestrator that runs Claude worker threads across
// your repositories.
package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"

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
	c.ResumeInterrupted() // threads that were working when Solo last closed carry on
	g := gui.New(gui.Live{Core: c, O: o})
	defer g.Close()

	light, dark := gui.Themes()
	a := app.New().AppID(appID()).
		// One Solo at a time: they would share the database. A second launch brings the first to the front.
		SingleInstance(nil).
		OnNotification(func(n app.NotificationActivation) { g.OpenFromNotification(n.Args) })
	g.Notify = func(n gui.Notification) {
		if err := app.Notify(app.Notification{Title: n.Title, Body: n.Body, Tag: "solo-" + shortTag(n.Thread), Args: n.Thread}); err != nil {
			log.Debug("notification", "err", err)
		}
	}
	g.ClearNotifications = func() {
		if err := app.ClearNotifications(); err != nil {
			log.Debug("clearing notifications", "err", err)
		}
	}
	for _, f := range gui.Fonts() {
		a = a.Font("", f.Weight, f.Data)
	}
	a.Run(app.Window{
		Title:      "Solo",
		Width:      1440,
		Height:     900,
		MinWidth:   1000,
		MinHeight:  600,
		LightTheme: light,
		DarkTheme:  dark,
		Root:       g.Root,
		Bindings:   gui.Bindings(),
		Chrome:     app.ChromeCustom, // Solo draws its own title bar
		Corners:    app.CornersRound,
	})
	return nil
}

// appID names Solo to the desktop. Windows ties it to the installer's Start menu shortcut, whose ID is
// jamb.json's identityName, which can't hold "_"; Linux uses it as the Wayland app ID and the desktop
// file's name.
func appID() string {
	if runtime.GOOS == "windows" {
		return "Solo.TylerBuilds"
	}
	return "io.github.tyler_builds.solo"
}

// shortTag keeps a notification tag within Windows's 64 characters: one per thread, so a newer
// notification replaces the thread's last.
func shortTag(threadID string) string {
	if threadID == "" {
		return "solo"
	}
	if len(threadID) > 36 {
		return threadID[:36]
	}
	return threadID
}
