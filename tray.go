package main

import (
	_ "embed"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"fyne.io/systray"
	"github.com/godbus/dbus/v5"
)

//go:embed icon.png
var iconPNG []byte

// trayAvailable reports whether the desktop has a StatusNotifier tray host
// (KDE, XFCE, Cinnamon, GNOME with the AppIndicator extension, ...).
func trayAvailable() bool {
	conn, err := dbus.SessionBus()
	if err != nil {
		return false
	}
	var has bool
	err = conn.BusObject().Call("org.freedesktop.DBus.NameHasOwner", 0, "org.kde.StatusNotifierWatcher").Store(&has)
	return err == nil && has
}

// runTray shows the tray icon until quit is called. Choosing Quit from the
// menu calls onQuit, which shuts the program down like Ctrl+C.
func runTray(o options, st *status, p *player, logPath string, onQuit func()) (quit func()) {
	url := playerURLs(o)[0]
	openURL := func(target string) {
		if err := exec.Command("xdg-open", target).Start(); err != nil {
			log.Printf("could not open %s: %v", target, err)
		}
	}

	onReady := func() {
		systray.SetIcon(iconPNG)
		systray.SetTitle("Disc Stream")
		systray.SetTooltip("Disc Stream – " + url)
		systray.SetOnTapped(func() { openURL(url) })

		state := systray.AddMenuItem("Starting…", "")
		state.Disable()
		addr := systray.AddMenuItem(url, "")
		addr.Disable()
		systray.AddSeparator()
		open := systray.AddMenuItem("Open player", "Open the web player in your browser")
		showLog := systray.AddMenuItem("Show log", logPath)
		if logPath == "" {
			showLog.Hide()
		}
		systray.AddSeparator()
		quitItem := systray.AddMenuItem("Quit", "Stop streaming and quit")

		go func() {
			tick := time.NewTicker(2 * time.Second)
			defer tick.Stop()
			for {
				select {
				case <-open.ClickedCh:
					openURL(url)
				case <-showLog.ClickedCh:
					openURL(logPath)
				case <-quitItem.ClickedCh:
					state.SetTitle("Stopping…")
					onQuit()
					return
				case <-tick.C:
					text := trayStatus(st, p)
					state.SetTitle(text)
					systray.SetTooltip("Disc Stream – " + text)
				}
			}
		}()
	}
	start, end := systray.RunWithExternalLoop(onReady, func() {})
	start()
	return end
}

// trayStatus is the one-line status shown in the tray menu.
func trayStatus(st *status, p *player) string {
	st.refresh(p)
	st.mu.Lock()
	defer st.mu.Unlock()
	name := ""
	var total float64
	for _, t := range st.Titles {
		if t.ID == st.Current {
			name, total = t.Name, t.Duration
		}
	}
	switch {
	case st.State == "error":
		return "Error: " + st.Detail
	case name == "":
		if st.Detail != "" {
			return fmt.Sprintf("%s (%s)", capitalize(st.State), st.Detail)
		}
		return capitalize(st.State)
	case st.Complete:
		return name + " – fully cached"
	case st.Ready:
		return fmt.Sprintf("%s – cached %s of %s", name,
			fmtDur(time.Duration(st.Cached*float64(time.Second))),
			fmtDur(time.Duration(total*float64(time.Second))))
	}
	return capitalize(st.State) + " – " + name
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
