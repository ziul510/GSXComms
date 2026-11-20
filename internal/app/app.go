// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-gui-org/go-gui/gui"
	"github.com/go-gui-org/go-gui/gui/backend"
	"github.com/ziul510/GSXCommsGo/internal/assets"
	"github.com/ziul510/GSXCommsGo/internal/commands"
	"github.com/ziul510/GSXCommsGo/internal/config"
	"github.com/ziul510/GSXCommsGo/internal/geo"
	"github.com/ziul510/GSXCommsGo/internal/gsx"
	"github.com/ziul510/GSXCommsGo/internal/speech"
)

var (
	bg          = gui.Hex(0x07080a)
	panel       = gui.Hex(0x0e1014)
	card        = gui.Hex(0x13151a)
	elevated    = gui.Hex(0x1a1c22)
	ink         = gui.Hex(0xe8eaee)
	muted       = gui.Hex(0x828995)
	disabledInk = gui.Hex(0x50555f)
	accent      = gui.Hex(0x39a4e3)
	accentHover = gui.Hex(0x60b8ec)
	terminal    = gui.Hex(0x050608)
	terminalInk = gui.Hex(0xc4c8d0)
	green       = gui.Hex(0x22d36a)
	red         = gui.Hex(0xff4d6a)
	orange      = gui.Hex(0xf97316)
)

type state struct {
	audioQueue, audioPriority                       chan playback
	saves                                           chan *saveRequest
	pendingSave                                     *saveRequest
	saved                                           chan struct{}
	debug                                           chan debugEvent
	location                                        geo.Position
	reader                                          *speech.Worker
	speaking                                        chan struct{}
	pttHeld, processing, speechOpen, savingKey      bool
	recording                                       context.CancelFunc
	recognizer                                      *speech.Worker
	dir                                             string
	cfg                                             config.Values
	host                                            *gui.App
	main                                            *gui.Window
	tray                                            *gui.SystemTrayHandle
	icon                                            []byte
	ctx                                             context.Context
	cancel                                          context.CancelFunc
	jobs                                            chan func(context.Context, *gsx.Session)
	done                                            chan struct{}
	lines                                           []string
	input                                           string
	ptt                                             string
	connected, ready, busy, overlay, speedKnown     bool
	menu                                            gsx.Menu
	speed                                           float64
	highlight                                       int
	settingsOpen, aboutOpen, accentOpen, hotfixOpen bool
	binding                                         func(config.Binding)
}

func Run(dir string) {
	settings, err := config.Load(dir)
	icon, _ := assets.Files.ReadFile("app.png")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &state{dir: dir, cfg: settings, icon: icon, host: gui.NewApp(), ctx: ctx, cancel: cancel, jobs: make(chan func(context.Context, *gsx.Session), 16), done: make(chan struct{}), ptt: "Idle", highlight: -1}
	s.recognizer = speech.NewWorker("sherpa")
	s.reader = speech.NewWorker("kokoro")
	s.speaking = make(chan struct{}, 1)
	s.audioQueue = make(chan playback, 16)
	s.audioPriority = make(chan playback, 8)
	go s.playbackLoop()
	defer s.reader.Reset()
	go restoreMute(ctx, dir)
	defer s.recognizer.Reset()
	s.host.ExitMode = gui.ExitOnMainClose
	s.saves = make(chan *saveRequest, 16)
	s.saved = make(chan struct{})
	s.debug = make(chan debugEvent, 128)
	go s.settingsWriter()
	go s.debugWriter()
	s.log("SYS", "Standby for sim connection")
	s.log("SYS", "Loading speech recognition")
	if err != nil {
		s.log("SYS", err.Error())
	}
	theme := gui.ThemeDark
	theme.ColorBackground = bg
	theme.ColorPanel = panel
	theme.ColorAccent = accent
	gui.SetTheme(theme)
	w := gui.NewWindow(gui.WindowCfg{
		Title: "GSXComms", Width: 420, Height: 680, MinWidth: 380, MinHeight: 560, State: s, IconPNG: icon,
		OnInit: func(w *gui.Window) {
			s.main = w
			w.TitlebarDark(true)
			w.SetView(mainView)
			go s.simulator()
			s.configureTray()
			s.setTopmost()
			captureKey.Store(bindingKey(config.Get[config.Binding](s.cfg, "ptt_bind")))
			go listenKeyboard(ctx, s.keyboardEvent)
			go s.pollController(ctx)
			go s.listenHID(ctx)
			if config.Get[bool](s.cfg, "start_minimized") {
				// Only hide the window if there is a tray icon to bring it back.
				if s.tray != nil {
					w.Hide()
				} else {
					w.Minimize()
				}
			}
			if !config.Get[bool](s.cfg, "accent_setup_complete") {
				s.openAccents()
			}
			if config.Get[bool](s.cfg, "auto_connect_sim") {
				s.toggleConnect()
			}
		},
		OnCloseRequest: func(w *gui.Window) { s.quit() },
	})
	backend.RunApp(s.host, w)
	// Give queued settings writes time to finish after the UI closes.
	close(s.saves)
	select {
	case <-s.saved:
	case <-time.After(2 * time.Second):
	}
	cancel()
	// The simulator loop may still be closing a menu it owns.
	if s.main != nil {
		select {
		case <-s.done:
		case <-time.After(2500 * time.Millisecond):
		}
	}
}

func (s *state) quit() {
	s.cancel()
	if s.tray != nil {
		s.host.RemoveSystemTray(s.tray)
		s.tray = nil
	}
	s.main.Close()
}

func (s *state) configureTray() {
	if !config.Get[bool](s.cfg, "minimize_to_tray") {
		if s.tray != nil {
			s.host.RemoveSystemTray(s.tray)
			s.tray = nil
		}
		return
	}
	if s.tray != nil {
		return
	}
	tray, err := s.host.SetSystemTray(gui.SystemTrayCfg{
		Tooltip: "GSXComms", IconPNG: s.icon,
		Menu: []gui.NativeMenuItemCfg{{ID: "show", Text: "Show"}, {ID: "quit", Text: "Quit"}},
		OnAction: func(id string) {
			s.main.QueueCommand(func(w *gui.Window) {
				switch id {
				case "show":
					w.Show()
					w.Restore()
				case "quit":
					s.quit()
				}
			})
		},
	})
	if err != nil {
		s.log("SYS", "Tray is unavailable")
		return
	}
	s.tray = tray
}

func (s *state) log(sender, message string) {
	for _, line := range strings.Split(strings.TrimSpace(message), "\n") {
		if line != "" {
			s.lines = append(s.lines, fmt.Sprintf("[%s]: %s", sender, line))
			if config.Get[bool](s.cfg, "debug_mode") {
				s.debugLog(debugEvent{true, fmt.Sprintf("[%s] %s: %s", time.Now().Format("15:04:05"), sender, line)})
			}
		}
	}
	// Bound the text sent to the conversation view on each redraw.
	if len(s.lines) > 1000 {
		s.lines = s.lines[len(s.lines)-1000:]
	}
	if s.main != nil {
		s.main.QueueCommand(func(w *gui.Window) { w.ScrollVerticalToPct("conversation", 1) })
	}
}

func (s *state) async(f func()) {
	// Background work hands state changes back to the UI thread here.
	s.main.QueueCommand(func(w *gui.Window) {
		if s.ctx.Err() == nil {
			f()
			w.InvalidateLayout()
		}
	})
}

func (s *state) addJob(action func(context.Context, *gsx.Session)) bool {
	select {
	case s.jobs <- action:
		return true
	default:
		s.log("SYS", "The GSX command queue is busy")
		return false
	}
}

func (s *state) simulator() {
	defer close(s.done)
	// Keep polling and commands on one goroutine since they share the dispatch queue.
	session := gsx.New(nil, gsx.Panel())
	defer session.Close()
	ticker := time.NewTicker(600 * time.Millisecond)
	defer ticker.Stop()
	var lastRemote, timeAtLocation time.Time
	var lastLocation geo.Position
	failed := 0
	for {
		select {
		case <-s.ctx.Done():
			return
		case job := <-s.jobs:
			ctx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
			if ctx.Err() == nil {
				job(ctx, session)
			}
			cancel()
			lastRemote = time.Now()
		case <-ticker.C:
			if !session.Connected() {
				continue
			}
			ctx, cancel := context.WithTimeout(s.ctx, 500*time.Millisecond)
			ready, speed, err := session.Status(ctx)
			cancel()
			if err != nil && !errors.Is(err, gsx.ErrSpeedUnavailable) {
				failed++
			} else {
				failed = 0
			}
			if failed >= 3 {
				_ = session.Close()
				failed = 0
				s.async(func() {
					s.connected = false
					s.ready = false
					s.overlay = false
					s.log("SYS", "Disconnected from SimConnect")
				})
				continue
			}
			menu := session.ReadMenu()
			// Accent lookup can poll less often than menu status and ground speed.
			if time.Since(timeAtLocation) > 5*time.Second {
				timeAtLocation = time.Now()
				geoCtx, stop := context.WithTimeout(s.ctx, 850*time.Millisecond)
				lat, lon, err := session.Location(geoCtx)
				stop()
				if err == nil {
					if pos, err := geo.Nearest(lat, lon); err == nil && pos != lastLocation {
						lastLocation = pos
						overrides := geo.Overrides(s.dir)
						s.async(func() {
							s.location = pos
							fallback := config.Get[string](s.cfg, "default_accent")
							forced := config.Get[string](s.cfg, "force_accent")
							if config.Get[bool](s.cfg, "debug_mode") {
								selected := geo.Accent(s.location, overrides, fallback, forced)
								s.debugLog(debugEvent{true, "GEO: country " + pos.Country + ", accent " + selected})
							}
						})
					}
				}
			}
			// Give control back to GSX when the app has been idle.
			if session.RemoteActive() && time.Since(lastRemote) >= 10*time.Second {
				ctx, cancel := context.WithTimeout(s.ctx, time.Second)
				_ = session.Release(ctx)
				cancel()
			}
			s.async(func() {
				s.ready = ready
				s.speedKnown = err == nil
				s.speed = speed
				if s.overlay {
					s.menu = menu
				}
			})
		}
	}
}

func (s *state) toggleConnect() {
	if s.busy {
		return
	}
	s.busy = s.addJob(func(ctx context.Context, session *gsx.Session) {
		if session.Connected() {
			_ = session.Close()
			s.async(func() {
				s.connected = false
				s.ready = false
				s.busy = false
				s.overlay = false
				s.log("SYS", "Disconnected from SimConnect")
			})
			return
		}
		sim, err := gsx.Connect(ctx)
		if err != nil {
			s.async(func() { s.busy = false; s.log("ERR", err.Error()) })
			return
		}
		session.Sim = sim
		ready, speed, statusErr := session.Status(ctx)
		s.async(func() {
			s.busy = false
			s.connected = true
			s.ready = ready
			s.speed = speed
			s.speedKnown = statusErr == nil
			s.log("SYS", "Connected to SimConnect")
			s.cue(s.cfg.Copy(), "connected", true)
		})
	})
}

func (s *state) toggleMenu() {
	if s.overlay {
		if s.addJob(func(ctx context.Context, session *gsx.Session) { _ = session.Release(ctx) }) {
			s.overlay = false
		}
		return
	}
	if !s.ready {
		return
	}
	s.addJob(func(ctx context.Context, session *gsx.Session) {
		if err := session.Open(ctx); err != nil {
			s.async(func() { s.log("GSX", err.Error()) })
			return
		}
		menu, err := session.WaitMenu(ctx)
		s.async(func() {
			if err != nil {
				s.log("GSX", err.Error())
				return
			}
			s.menu = menu
			s.overlay = true
		})
	})
}

func (s *state) choose(index int) {
	if index < 0 || index >= len(s.menu.Options) {
		s.log("SYS", fmt.Sprintf("GSX menu has no item %d", index+1))
		return
	}
	title, label := s.menu.Title, s.menu.Options[index]
	if s.addJob(func(ctx context.Context, session *gsx.Session) {
		// GSX can replace the menu while a selection waits in the job queue.
		current := session.ReadMenu()
		if current.Title != title || index >= len(current.Options) || current.Options[index] != label {
			s.async(func() {
				s.highlight = -1
				s.menu = current
				s.log("GSX", "The GSX menu changed before selection")
			})
			return
		}
		err := session.Choose(ctx, index)
		menu := session.ReadMenu()
		s.async(func() {
			if err != nil {
				s.log("GSX", err.Error())
			}
			s.menu = menu
		})
		time.AfterFunc(time.Second, func() {
			s.async(func() {
				if s.highlight == index {
					s.highlight = -1
				}
			})
		})
	}) {
		s.highlight = index
		s.log("GSX", fmt.Sprintf("Menu item %d selected by number", index+1))
	}
}

func (s *state) transmissionBlock() string {
	if config.Get[bool](s.cfg, "simulation_mode") || config.Get[bool](s.cfg, "ignore_speed_restriction") {
		return ""
	}
	if !s.speedKnown {
		return "Taxi speed unavailable for transmission"
	}
	if s.speed > 50 {
		return "Taxi speed too high for transmission"
	}
	return ""
}

func (s *state) submit(input string) {
	input = strings.TrimSpace(input)
	if input == "" {
		return
	}
	s.log("CMD", input)
	prefs := s.cfg.Copy()
	if !s.connected && !config.Get[bool](prefs, "simulation_mode") {
		if readoutMatches(prefs, input) {
			s.log("SYS", "SimConnect handshake required to read the GSX menu")
		} else {
			s.log("SYS", "SimConnect handshake required")
		}
		return
	}
	// Typed commands pass through the same speed gate as PTT.
	if reason := s.transmissionBlock(); reason != "" {
		s.log("SYS", reason)
		return
	}
	// A bare number is a direct selection only while the menu is visible.
	if idx, ok := commands.Number(input); ok && s.overlay {
		s.choose(idx)
		return
	}
	s.addJob(func(ctx context.Context, session *gsx.Session) {
		words, err := commands.Load(s.dir)
		if err != nil {
			s.async(func() { s.log("ERR", "Command vocabulary is unreadable") })
			return
		}
		raw := strings.ToLower(input)
		text := words.Normalize(raw)
		if readoutMatches(prefs, text, raw) {
			s.sayMenu(ctx, session, prefs)
			return
		}
		action := words.Route(text, raw, config.Get[[]config.Preset](prefs, "presets"), config.Get[bool](prefs, "enable_menu_matching"))
		if config.Get[bool](prefs, "enable_gsx_menu_text_correction") {
			// A current menu label can take precedence over the vocabulary route.
			if text2, _ := commands.CorrectMenu(text, session.ReadMenu().Options); text2 != "" {
				text = text2
				action = commands.Action{Kind: "menu"}
			}
		}
		auto := config.Get[bool](prefs, "auto_select_operator")
		log := func(sender, msg string) { s.async(func() { s.log(sender, msg) }) }
		execute := func(service string) bool {
			// Each preset service gets its own deadline even if earlier ones ran long.
			serviceCtx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
			defer cancel()
			selected, err := session.Execute(serviceCtx, text, service, words.Keywords(service), auto)
			if errors.Is(err, gsx.ErrHotfix) {
				s.async(func() { s.showHotfix() })
				return false
			}
			if err != nil {
				log("GSX", err.Error())
				return false
			}
			log("GSX", fmt.Sprintf("Menu option %q matched and selected", selected))
			if service != "" {
				s.cue(prefs, service, true)
			}
			return true
		}
		switch action.Kind {
		case "preset":
			log("PRESET", fmt.Sprintf("Preset %q triggered", action.Preset))
			done := 0
			seen := map[string]bool{}
			for _, service := range action.Services {
				if s.ctx.Err() != nil {
					return
				}
				if !seen[service] && execute(service) {
					done++
					seen[service] = true
				}
			}
			log("PRESET", fmt.Sprintf("Preset %q complete: %d/%d services ok", action.Preset, done, len(action.Services)))
		case "service":
			execute(action.Services[0])
		case "menu":
			execute("")
		case "greeting":
			s.cue(prefs, "greeting", false)
			log("GSX", "Ground crew greeting")
		case "ambiguous":
			s.cue(prefs, "error", true)
			log("SYS", "Multiple cmds detected")
		default:
			log("SYS", "No command recognised")
			s.cue(prefs, "error", true)
		}
	})
}

func readNotice(name string) string {
	exe, _ := os.Executable()
	data, err := os.ReadFile(filepath.Join(filepath.Dir(exe), name))
	if err != nil {
		data, _ = os.ReadFile(name)
	}
	return string(data)
}
