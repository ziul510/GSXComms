// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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

// UI state stays on the window thread Workers communicate through queues and callbacks
type state struct {
	audioQueue                                       []playback
	audioWake                                        chan struct{}
	clipCache                                        []cachedClip
	audioMu                                          sync.Mutex
	audioCancel                                      context.CancelFunc
	crewMute                                         crewMute
	saves                                            chan *saveRequest
	pendingSave                                      *saveRequest
	saved                                            chan struct{}
	debug                                            chan debugEvent
	debugEnabled                                     atomic.Bool
	logSecret                                        *regexp.Regexp
	location                                         atomic.Pointer[geo.Position]
	accentsDir, crewVoice                            string
	reader                                           *speech.Worker
	speaking                                         chan struct{}
	pttHeld, processing, savingKey, settingUpMistral bool
	recording                                        context.CancelFunc
	recognizer                                       *speech.Worker
	sttMu                                            sync.Mutex
	speechReady                                      bool
	speechStatus                                     string
	speechLoading                                    string
	speechLoadCancel                                 context.CancelFunc
	speechGeneration                                 uint64
	speechAttempted                                  map[string]bool
	speechChanged                                    func(string)
	dir                                              string
	cfg                                              config.Values
	host                                             *gui.App
	main                                             *gui.Window
	tray                                             *gui.SystemTrayHandle
	dialogs                                          map[uint64]func()
	dialogID                                         uint64
	icon                                             []byte
	ctx                                              context.Context
	cancel                                           context.CancelFunc
	jobs                                             chan func(context.Context, *gsx.Session)
	done                                             chan struct{}
	lines                                            []string
	input                                            string
	ptt                                              string
	connected, ready, busy, overlay                  bool
	menu                                             gsx.Menu
	menuWatch                                        bool // owned by the simulator loop
	menuActivity                                     time.Time
	lastConnect                                      time.Time
	servicesMu                                       sync.Mutex
	services                                         map[string]bool
	groundSpeed                                      atomic.Pointer[speedReading]
	abovePTTAltitude                                 atomic.Bool
	policy                                           atomic.Pointer[transmissionPolicy]
	highlights                                       []menuHighlight
	vocabulary                                       commands.Loader
	settingsOpen, accentOpen, hotfixOpen             bool
	binding                                          func(config.Binding)
	hidPaths                                         map[string][]string
	heldHIDPath                                      string
	heldHIDButton                                    int
}

func Run(dir string) {
	initErr := config.InitializeSettings(dir)
	settings, err := config.Load(dir)
	icon, _ := assets.Files.ReadFile("app.png")
	archives := assetDir("Accents")
	theme := gui.ThemeDark
	theme.ColorBackground = bg
	theme.ColorPanel = panel
	theme.ColorAccent = accent
	gui.SetTheme(theme)
	if !config.Get[bool](settings, "accent_setup_complete") {
		if restoreAccentSetup(filepath.Join(dir, "Accents"), settings) {
			err = errors.Join(err, config.Save(dir, settings))
		} else {
			settings = runFirstAccentSetup(dir, archives, settings, icon)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &state{dir: dir, cfg: settings, icon: icon, host: gui.NewApp(), ctx: ctx, cancel: cancel, jobs: make(chan func(context.Context, *gsx.Session), 16), done: make(chan struct{}), ptt: "Idle"}
	s.accentsDir = archives
	s.crewVoice = []string{"female", "male"}[rand.IntN(2)]
	s.services = make(map[string]bool)
	// Use separate native workers for input transcription and menu readout
	s.recognizer = speech.NewWorker("sherpa")
	s.speechAttempted = make(map[string]bool)
	s.reader = speech.NewWorker("kokoro")
	s.speaking = make(chan struct{}, 1)
	s.audioWake = make(chan struct{}, 1)
	// Keep all local audio on one playback loop so it can share the mute watchdog
	go s.playbackLoop()
	defer s.reader.Reset()
	// Start checking for a Couatl mute left by an earlier process
	go restoreMute(ctx, dir)
	defer s.recognizer.Reset()
	// Closing a settings dialog should leave the main application running
	s.host.ExitMode = gui.ExitOnMainClose
	s.saves = make(chan *saveRequest, 16)
	s.saved = make(chan struct{})
	s.debug = make(chan debugEvent, 128)
	go s.settingsWriter()
	go s.debugWriter()
	key, _ := config.ReadKey()
	s.setLogSecret(key)
	s.setDebug(config.Get[bool](s.cfg, "debug_mode"))
	s.log("SYS", "Standby for sim connection")
	if initErr != nil {
		s.log("ERR", initErr.Error())
	}
	if err != nil {
		s.log("SYS", err.Error())
	}
	// Check that recordings for the configured packs are available on startup.
	if err := installAccentPacks(ctx, archives, filepath.Join(dir, "Accents"), config.Get[[]string](settings, "installed_packs")); err != nil {
		s.log("ERR", err.Error())
	}
	w := gui.NewWindow(gui.WindowCfg{
		Title: "GSXComms", Width: 420, Height: 680, MinWidth: 380, MinHeight: 560, State: s, IconPNG: icon,
		OnInit: func(w *gui.Window) {
			s.main = w
			w.SetView(mainView)
			// Start background work after the main window can accept UI callbacks
			go s.simulator()
			s.prepareSpeech(true, false)
			s.configureTray()
			s.setTopmost()
			// Load the active PTT key before the keyboard hook starts listening
			captureKey.Store(bindingKey(config.Get[config.Binding](s.cfg, "ptt_bind")))
			go listenKeyboard(ctx, s.keyboardEvent, s.minimizeToTray)
			go s.pollController(ctx)
			go s.listenHID(ctx)
			if config.Get[bool](s.cfg, "start_minimized") {
				// Only hide the window if there is a tray icon to bring it back
				if s.tray != nil {
					w.Hide()
				}
			}
			if config.Get[bool](s.cfg, "auto_connect_sim") {
				s.toggleConnect()
			}
		},
		OnCloseRequest: func(w *gui.Window) { s.quit() },
	})
	// Request a dark caption before the backend shows the window
	w.TitlebarDark(true)
	backend.RunApp(s.host, w)
	// Give queued settings writes time to finish after the UI closes
	close(s.saves)
	select {
	case <-s.saved:
	case <-time.After(2 * time.Second):
	}
	cancel()
	s.crewMute.Lock()
	s.crewMute.stop()
	s.crewMute.Unlock()
	// The simulator loop may still be closing a menu it owns
	if s.main != nil {
		select {
		case <-s.done:
		case <-time.After(2500 * time.Millisecond):
		}
	}
}

func runFirstAccentSetup(dir, archives string, prefs config.Values, icon []byte) config.Values {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := firstAccentWindow(ctx, dir, archives, prefs, func(saved config.Values) {
		prefs = saved
	})
	cfg.IconPNG = icon
	w := gui.NewWindow(cfg)
	w.TitlebarDark(true)
	backend.RunApp(gui.NewApp(), w)
	return prefs
}

func firstAccentWindow(ctx context.Context, dir, archives string, prefs config.Values, onSaved func(config.Values)) gui.WindowCfg {
	recordings := filepath.Join(dir, "Accents")
	var saved config.Values
	view := accentSetupView(ctx, archives, recordings, prefs, func(enabled []string, fallback string, then func(error)) {
		saved = accentPreferences(prefs.Copy(), enabled, fallback)
		go func() { then(config.Save(dir, saved)) }()
	}, func(w *gui.Window) {
		onSaved(saved)
		w.Close()
	})
	cfg := accentWindow()
	cfg.OnInit = func(w *gui.Window) {
		w.SetView(view)
	}
	cfg.OnCloseRequest = func(w *gui.Window) { w.Close() }
	return cfg
}

// Cancel background work before closing the main window and dropping its tray actions
func (s *state) quit() {
	s.cancel()
	if s.tray != nil {
		s.host.RemoveSystemTray(s.tray)
		s.tray = nil
	}
	s.main.Close()
}

func (s *state) configureTray() {
	// Disabling the tray option also removes an icon created by an earlier save
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
		// Tray callbacks queue their window changes on the UI thread
		OnAction: func(id string) {
			s.main.QueueCommand(func(w *gui.Window) {
				switch id {
				case "", "show":
					w.Show()
					w.Restore()
					focusWindow("GSXComms")
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

func (s *state) minimizeToTray(hwnd uintptr) {
	if hwnd != windowByTitle("GSXComms") {
		return
	}
	s.async(func() {
		if s.tray == nil {
			return
		}
		for _, closeDialog := range s.dialogs {
			closeDialog()
		}
		s.main.Hide()
	})
}

func (s *state) setLogSecret(key string) {
	if key == "" {
		s.logSecret = nil
	} else {
		s.logSecret = regexp.MustCompile("(?i)" + regexp.QuoteMeta(key))
	}
}

func (s *state) log(sender, message string) {
	if s.logSecret != nil {
		message = s.logSecret.ReplaceAllString(message, "[redacted]")
	}
	// Persist only the event category for transcript rows
	for _, line := range strings.Split(strings.TrimSpace(message), "\n") {
		if line != "" {
			s.lines = append(s.lines, fmt.Sprintf("[%s]: %s", sender, line))
			if sender == "ERR" {
				s.trace("ERR", "An application error appeared in the conversation")
			}
		}
	}
	// Bound the text sent to the conversation view on each redraw
	if len(s.lines) > 1000 {
		s.lines = s.lines[len(s.lines)-1000:]
	}
	if s.main != nil {
		// Scroll after the UI has a chance to lay out the appended text
		s.main.QueueCommand(func(w *gui.Window) { w.ScrollVerticalToPct("conversation", 1) })
	}
}

func (s *state) async(f func()) {
	// Background work hands state changes back to the UI thread here
	s.main.QueueCommand(func(w *gui.Window) {
		if s.ctx.Err() == nil {
			f()
			w.InvalidateLayout()
		}
	})
}

// Keep the UI responsive when the simulator can't drain the command queue
func (s *state) addJob(action func(context.Context, *gsx.Session)) bool {
	select {
	case s.jobs <- action:
		return true
	default:
		const message = "The GSX command queue is busy"
		s.log("SYS", message)
		if s.main != nil {
			s.main.Toast(gui.ToastCfg{Title: message})
		}
		s.trace("GSX", "Command rejected because the queue is full")
		return false
	}
}

type menuHighlight struct {
	page  gsx.Menu
	index int
	until time.Time
}

// One lane owns all menu operations and decisions about remote control
func (s *state) simulator() {
	defer close(s.done)
	session := gsx.New(nil, gsx.Panel())
	defer session.Close()
	ticker := time.NewTicker(600 * time.Millisecond)
	defer ticker.Stop()
	var lastMenu gsx.Menu
	var lastReopen time.Time
	var pollDone chan struct{}
	pollCtx, stopPoll := context.WithCancel(s.ctx)
	defer func() {
		stopPoll()
		if pollDone != nil {
			<-pollDone
		}
	}()
	session.OnReadError = func(error) {
		s.trace("GSX", "Failed to read the panel menu")
		s.async(func() { s.log("GSX", "Could not read panel menu") })
	}
	session.OnChoice = func(page gsx.Menu, index int) {
		s.trace("GSX", fmt.Sprintf("Selected slot %d of %d", index+1, len(page.Options)))
		s.menuActivity = time.Now()
		s.async(func() {
			if !s.overlay {
				return
			}
			s.highlights = append(s.highlights, menuHighlight{page, index, time.Now().Add(time.Second)})
			time.AfterFunc(time.Second, func() {
				s.async(func() {
					s.highlights = slices.DeleteFunc(s.highlights, func(h menuHighlight) bool {
						return !time.Now().Before(h.until)
					})
				})
			})
		})
	}
	for {
		// Prefer retiring an offline connection before processing more jobs
		select {
		case <-pollDone:
			_ = session.Close()
			pollDone = nil
			s.location.Store(nil)
			lastMenu, lastReopen, s.menuActivity = gsx.Menu{}, time.Time{}, time.Time{}
			s.menuWatch = false
			s.resetServices()
		default:
		}
		select {
		case <-s.ctx.Done():
			return
		case <-pollDone:
			continue
		case job := <-s.jobs:
			ctx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
			if ctx.Err() == nil {
				job(ctx, session)
			}
			cancel()
			if session.Connected() && pollDone == nil {
				pollDone = make(chan struct{})
				go s.pollSimulator(pollCtx, session.Sim, pollDone)
			}
		case <-ticker.C:
			if !session.Connected() {
				s.async(s.retryConnection)
				continue
			}
			menu := session.ReadMenu()
			if session.RemoteActive() {
				if len(menu.Options) > 0 && !menu.Same(lastMenu) {
					lastMenu, s.menuActivity = menu, time.Now()
				}
				switch {
				case time.Since(s.menuActivity) >= 10*time.Second:
					s.trace("GSX", "Releasing the idle menu")
					ctx, cancel := context.WithTimeout(s.ctx, 8*time.Second)
					if s.menuWatch {
						page, err := session.RefreshAndRelease(ctx)
						if len(page.Options) > 0 {
							menu = page
						}
						if err != nil {
							s.traceFailure("GSX", "Could not refresh and release the idle menu")
						}
					} else {
						_ = session.Release(ctx)
					}
					cancel()
				case s.menuWatch && len(menu.Options) == 0 && time.Since(lastReopen) >= 2*time.Second:
					lastReopen = time.Now()
					ctx, cancel := context.WithTimeout(s.ctx, 2*time.Second)
					if err := session.Open(ctx); err == nil {
						menu = session.ReadMenu()
					}
					cancel()
				}
			}
			if len(menu.Options) > 0 {
				s.async(func() {
					if s.overlay {
						s.menu = menu
					}
				})
			}
		}
	}
}

// Telemetry shares the transport one exchange at a time, independently of preset timing
func (s *state) pollSimulator(ctx context.Context, sim gsx.Transport, done chan struct{}) {
	defer close(done)
	defer s.groundSpeed.Store(nil)
	defer s.abovePTTAltitude.Store(false)
	session := gsx.New(sim, "")
	ticker := time.NewTicker(600 * time.Millisecond)
	defer ticker.Stop()
	var lastGeoPoll time.Time
	var geoLat, geoLon float64
	var high, fast bool
	failed := 0
	for ctx.Err() == nil {
		statusCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 500*time.Millisecond)
		ready, speed, err := session.Status(statusCtx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			s.groundSpeed.Store(&speedReading{speed, time.Now()})
		} else {
			s.groundSpeed.Store(nil)
		}
		if err != nil && !errors.Is(err, gsx.ErrSpeedUnavailable) {
			failed++
		} else {
			failed = 0
		}
		if failed >= 3 || errors.Is(err, gsx.ErrDisconnected) {
			s.traceFailure("GSX", "SimConnect poll lost the connection")
			s.async(func() {
				s.connected, s.ready, s.overlay = false, false, false
				s.menu, s.highlights = gsx.Menu{}, nil
				s.log("SYS", "Disconnected from SimConnect")
			})
			return
		}
		if err != nil {
			s.trace("GSX", "SimConnect poll did not return a valid speed")
		}
		altCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 420*time.Millisecond)
		altitude, altErr := session.Altitude(altCtx)
		stop()
		if ctx.Err() != nil {
			return
		}
		recovered, descended := err == nil && fast && speed <= 50, altErr == nil && high && altitude <= 10000
		if err == nil {
			fast = speed > 50
		}
		if altErr == nil {
			above := altitude > 10000
			if above && !high {
				s.async(func() { s.log("SYS", "Cruise: crew will refresh on descent") })
			}
			high = above
			s.abovePTTAltitude.Store(high)
		}
		// Redraw after both flight readings have been updated
		s.async(func() { s.ready = ready })
		if recovered || descended {
			// The next ground interaction starts with fresh service state and local voices.
			s.resetServices()
			lastGeoPoll = time.Time{}
			s.location.Store(nil)
			s.async(func() {
				s.randomizeVoice()
				s.log("SYS", "Ground service state reset. Crew refreshed")
			})
		}
		interval, movement := 5*time.Second, 0.001
		// A local crew changes less often in flight than it does while taxiing.
		if altitude > 3000 || speed > 80 {
			interval, movement = 30*time.Second, 0.02
		}
		if time.Since(lastGeoPoll) >= interval {
			lastGeoPoll = time.Now()
			geoCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 850*time.Millisecond)
			lat, lon, err := session.Location(geoCtx)
			stop()
			if err == nil && (s.location.Load() == nil || math.Abs(lat-geoLat) > movement || math.Abs(lon-geoLon) > movement) {
				if pos, err := geo.Nearest(lat, lon); err == nil {
					geoLat, geoLon = lat, lon
					s.location.Store(&pos)
					s.trace("GEO", "Updated aircraft country and region")
				}
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Manual and automatic attempts share the same cooldown.
func (s *state) retryConnection() {
	if config.Get[bool](s.cfg, "auto_connect_sim") && time.Since(s.lastConnect) >= 15*time.Second {
		s.toggleConnect()
	}
}

func (s *state) toggleConnect() {
	// Ignore another connect click until the queued operation has finished
	if s.busy || s.connected {
		return
	}
	s.lastConnect = time.Now()
	// Only show the connecting state if the job was accepted
	s.busy = s.addJob(func(ctx context.Context, session *gsx.Session) {
		sim, err := gsx.Connect(ctx)
		if err != nil {
			s.traceFailure("GSX", "SimConnect handshake failed")
			s.async(func() { s.busy = false; s.log("ERR", err.Error()) })
			return
		}
		// A successful SimConnect handshake and GSX startup are separate states
		session.Sim = sim
		s.trace("GSX", "SimConnect handshake succeeded")
		s.async(func() {
			s.busy = false
			s.connected = true
			s.log("SYS", "Connected to SimConnect")
		})
	})
}

func (s *state) toggleMenu() {
	// Leave the overlay visible if the queued close couldn't be scheduled
	if s.overlay {
		if s.addJob(func(ctx context.Context, session *gsx.Session) {
			s.menuWatch = false
			if session.RemoteActive() {
				_ = session.Release(ctx)
			}
		}) {
			s.overlay = false
		}
		return
	}
	if !s.ready {
		return
	}
	if s.addJob(func(ctx context.Context, session *gsx.Session) {
		s.menuWatch = true
		s.menuActivity = time.Now()
		menu, err := session.OpenMenuPage(ctx)
		s.async(func() {
			if err != nil {
				s.log("GSX", err.Error())
				return
			}
			if s.overlay {
				s.menu = menu
			}
		})
	}) {
		s.menu, s.highlights, s.overlay = gsx.Menu{}, nil, true
	}
}

// selectNumber runs on the simulator loop so it can reacquire the same page after idle release.
func (s *state) selectNumber(ctx context.Context, session *gsx.Session, page gsx.Menu, index int, prefs config.Values) {
	if !s.menuWatch {
		return
	}
	if index < 0 || index >= len(page.Options) {
		s.async(func() {
			s.log("SYS", fmt.Sprintf("GSX menu has no item %d", index+1))
			s.cue(prefs, "error", "", true)
		})
		return
	}
	s.menuActivity = time.Now()
	current := session.ReadMenu()
	var err error
	if !session.RemoteActive() || len(current.Options) == 0 {
		current, err = session.OpenMenuPage(ctx)
	}
	if err == nil {
		err = session.ChooseOn(ctx, page, index)
		if err != nil {
			current = session.ReadMenu()
		}
	}
	s.async(func() {
		if err != nil {
			s.log("GSX", err.Error())
			s.cue(prefs, "error", "", true)
			if s.overlay && (errors.Is(err, gsx.ErrMenuChanged) || len(current.Options) > 0) {
				s.menu = current
			}
			return
		}
		s.log("GSX", fmt.Sprintf("Menu item %d selected by number", index+1))
	})
}

const speedFreshness = 2 * time.Second

type speedReading struct {
	knots float64
	at    time.Time
}

type transmissionPolicy struct {
	simulation, ignoreSpeed, speechReady bool
}

func (s *state) publishTransmissionPolicy() {
	s.policy.Store(&transmissionPolicy{
		simulation:  config.Get[bool](s.cfg, "simulation_mode"),
		ignoreSpeed: config.Get[bool](s.cfg, "ignore_speed_restriction"),
		speechReady: s.speechReady,
	})
}

func (r *speedReading) fresh() bool {
	return r != nil && time.Since(r.at) <= speedFreshness
}

func (s *state) speedBlock(simulation, ignoreSpeed bool) string {
	if simulation || ignoreSpeed {
		return ""
	}
	if speed := s.groundSpeed.Load(); speed.fresh() {
		if speed.knots > 50 {
			return "Taxi speed too high for transmission"
		}
		return ""
	}
	return "Taxi speed unavailable for transmission"
}

func (s *state) loadVocabulary() commands.Vocabulary {
	vocab, warning := s.vocabulary.Load(s.dir)
	if warning != "" {
		s.async(func() {
			s.log("ERR", warning)
			s.main.Toast(gui.ToastCfg{Title: warning})
		})
	}
	return vocab
}

func (s *state) submit(input string) {
	input = strings.TrimSpace(input)
	if input == "" {
		return
	}
	// Snapshot window state before handing the request to the simulator loop.
	prefs := s.cfg.Copy()
	page, overlay := s.menu, s.overlay
	s.publishTransmissionPolicy()
	fail := func(reason string) {
		s.async(func() {
			s.log("CMD", input)
			s.log("SYS", reason)
			s.cue(prefs, "error", "", true)
		})
	}
	s.addJob(func(ctx context.Context, session *gsx.Session) {
		words := s.loadVocabulary()
		raw := strings.ToLower(input)
		text := words.Normalize(raw)
		// Typed readouts need the simulator and Kokoro, but never require a recognizer.
		if readoutMatches(prefs, text, raw) {
			s.async(func() { s.log("CMD", "GSX menu readout requested") })
			s.sayMenu(ctx, session, prefs)
			return
		}
		gate := func() error {
			policy := s.policy.Load()
			if !session.Connected() && !policy.simulation {
				return gsx.ErrDisconnected
			}
			if blocked := s.speedBlock(policy.simulation, policy.ignoreSpeed); blocked != "" {
				return errors.New(blocked)
			}
			if !policy.speechReady {
				return errors.New("Speech recognition is unavailable or still loading")
			}
			return nil
		}
		if err := gate(); err != nil {
			fail(err.Error())
			return
		}
		session.BeforeChoice = gate
		defer func() { session.BeforeChoice = nil }()
		if idx, ok := commands.Number(text); ok && overlay {
			s.async(func() { s.log("CMD", input) })
			s.selectNumber(ctx, session, page, idx, prefs)
			return
		}
		s.menuActivity = time.Now()
		log := func(sender, msg string) { s.async(func() { s.log(sender, msg) }) }
		action := words.Route(text, raw, config.Get[[]config.Preset](prefs, "presets"), config.Get[bool](prefs, "enable_menu_matching"))
		corrected := false
		// Apply menu correction only after routing known commands
		if action.Kind == "menu" && session.Connected() {
			page := session.ReadMenu()
			opened := len(page.Options) == 0
			if opened {
				page, _ = session.OpenMenuPage(ctx)
			}
			if text2, _ := commands.CorrectMenu(text, page.Options); text2 != "" {
				log("CMD", fmt.Sprintf("%s → %s", text, text2))
				text = text2
				corrected = true
			}
			if err := gate(); err != nil {
				s.trace("GSX", "Transmission blocked after text correction")
				if opened && session.RemoteActive() {
					_ = session.Release(ctx)
				}
				fail(err.Error())
				return
			}
			if !corrected && opened && session.RemoteActive() {
				_ = session.Release(ctx)
			}
		}
		if !corrected {
			log("CMD", input)
		}
		auto := config.Get[bool](prefs, "auto_select_operator")
		if action.Kind == "preset" || action.Kind == "service" || corrected {
			defer s.muteCrew(ctx, prefs)()
		}
		execute := func(service string) bool {
			// Each preset service gets its own deadline even if earlier ones ran long
			serviceCtx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
			defer cancel()
			if err := gate(); err != nil {
				log("SYS", err.Error())
				return false
			}
			selected, err := session.Execute(serviceCtx, text, service, words.Keywords(service), auto)
			if selected != "" {
				matched := service
				if matched == "" {
					matched = menuService(selected)
				}
				s.acknowledge(prefs, matched, selected, service != "" || corrected)
			}
			// The stuck navigation menu needs the GSX hotfix prompt
			if errors.Is(err, gsx.ErrHotfix) {
				s.async(func() { s.showHotfix() })
				return false
			}
			if err != nil {
				if errors.Is(err, gsx.ErrMenuChanged) {
					page := session.ReadMenu()
					s.async(func() {
						if s.overlay {
							s.menu = page
						}
					})
				}
				log("GSX", err.Error())
				return false
			}
			log("GSX", fmt.Sprintf("Menu option %q matched and selected", selected))
			return true
		}
		switch action.Kind {
		case "preset":
			log("PRESET", fmt.Sprintf("Preset %q triggered", action.Preset))
			done := 0
			// A duplicate preset entry may retry a failed service but won't repeat a successful one
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
			if !execute(action.Services[0]) {
				s.async(func() { s.cue(prefs, "error", "", true) })
			}
		case "menu":
			execute("")
		case "greeting":
			s.async(func() { s.cue(prefs, "greeting", "", false) })
			log("GSX", "Ground crew greeting")
		// Play the error cue and leave GSX alone when several services match
		case "ambiguous":
			s.async(func() { s.cue(prefs, "error", "", true) })
			log("SYS", "Multiple cmds detected")
		default:
			log("SYS", "No command recognised")
			s.async(func() { s.cue(prefs, "error", "", true) })
		}
	})
}

func menuService(label string) string {
	label = strings.NewReplacer("-", "", "_", "").Replace(strings.ToUpper(label))
	for _, service := range []string{"GPU", "JETWAY", "STAIRS", "DEBOARDING", "BOARDING", "CATERING", "CLEANING", "DEICE", "LAVATORY", "PUSHBACK", "REFUELING", "WATER"} {
		if strings.Contains(label, service) {
			return service
		}
	}
	return ""
}
