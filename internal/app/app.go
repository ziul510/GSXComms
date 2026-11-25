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
	readoutGeneration                                atomic.Uint64
	keyboardUnavailable                              bool
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
	readouts                                         readoutScope
	pttHeld, processing, savingKey, settingUpMistral bool
	recording                                        context.CancelCauseFunc
	recognizer                                       *speech.Worker
	sttMu                                            sync.Mutex
	speechReady                                      bool
	speechStatus                                     string
	speechLoading                                    string
	speechLoadCancel                                 context.CancelFunc
	speechGeneration                                 uint64
	speechAttempted                                  map[string]bool
	speechChanged                                    func(string)
	presetReadoutChanged                             func()
	dir                                              string
	cfg                                              config.Values
	host                                             *gui.App
	main                                             *gui.Window
	tray                                             *gui.SystemTrayHandle
	dialogs                                          map[uint64]dialogWindow
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
	overlayDismissal                                 uint64
	menuWatch                                        bool // owned by the simulator loop
	menuRetryAt                                      time.Time
	operatorMenu                                     gsx.Menu // owned by the simulator loop
	operatorSubmitted                                time.Time
	menuActivity                                     time.Time
	lastConnect                                      time.Time
	servicesMu                                       sync.Mutex
	services                                         map[string]bool
	groundSpeed                                      atomic.Pointer[speedReading]
	pttAltitude                                      atomic.Pointer[float64]
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
	s.readouts = newReadoutScope(ctx)
	s.audioWake = make(chan struct{}, 1)
	// Keep all local audio on one playback loop so it can share the mute watchdog
	go s.playbackLoop()
	defer s.reader.Reset()
	defer s.recognizer.Reset()
	// Closing a settings dialog should leave the main application running
	s.host.ExitMode = gui.ExitOnMainClose
	s.saves = make(chan *saveRequest, 16)
	s.saved = make(chan struct{})
	s.debug = make(chan debugEvent, 128)
	go s.settingsWriter()
	go s.debugWriter(startDebugTerminal)
	key, _ := config.ReadKey()
	s.setLogSecret(key)
	s.setDebug(config.Get[bool](s.cfg, "debug_mode"))
	s.log("SYS", "Standby for sim connection")
	if initErr != nil {
		s.traceFailure("SYS", "Settings initialization failed", initErr)
		s.log("ERR", initErr.Error())
	}
	if err != nil {
		s.traceFailure("SYS", "Settings load or migration reported a problem", err)
		s.log("SYS", err.Error())
	}
	// Check that recordings for the configured packs are available on startup
	if err := installAccentPacks(ctx, archives, filepath.Join(dir, "Accents"), config.Get[[]string](settings, "installed_packs")); err != nil {
		s.traceFailure("AUDIO", "Accent pack check failed", err)
		s.log("ERR", err.Error())
	}
	var stopDialogPlacement func()
	restoreDone := make(chan struct{})
	w := gui.NewWindow(gui.WindowCfg{
		Title: "GSXComms", Width: 420, Height: 680, MinWidth: 380, MinHeight: 560, State: s, IconPNG: icon,
		OnInit: func(w *gui.Window) {
			s.main = w
			w.SetView(mainView)
			// Start background work after the main window can accept UI callbacks
			go s.simulator()
			// Recovery can report its status once the window is available.
			go func() {
				defer close(restoreDone)
				s.restoreMute(ctx)
			}()
			s.prepareSpeech(true, false)
			s.configureTray()
			s.setTopmost()
			stopDialogPlacement = installDialogPlacement()
			// Load the active PTT key before the keyboard hook starts listening
			captureKey.Store(bindingKey(config.Get[config.Binding](s.cfg, "ptt_bind")))
			go func() {
				if err := listenKeyboard(ctx, s.keyboardEvent); err != nil {
					s.async(func() {
						s.keyboardUnavailable = true
						if config.Get[config.Binding](s.cfg, "ptt_bind").Type == "key" {
							s.stopRecording(err)
						}
						s.traceFailure("PTT", "Keyboard hook stopped", err)
						s.log("ERR", "Keyboard push-to-talk unavailable. Restart GSXComms or bind a controller button.")
						s.host.Broadcast(func(w *gui.Window) { w.InvalidateLayout() })
					})
				}
			}()
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
		OnEvent:        s.mainWindowEvent,
		OnCloseRequest: func(w *gui.Window) { s.quit() },
	})
	// Request a dark caption before the backend shows the window
	w.TitlebarDark(true)
	// Keep the native placement hook scoped to the main window's event loop
	defer func() {
		if stopDialogPlacement != nil {
			stopDialogPlacement()
		}
	}()
	backend.RunApp(s.host, w)
	// Give queued settings writes time to finish after the UI closes
	close(s.saves)
	select {
	case <-s.saved:
	case <-time.After(2 * time.Second):
	}
	cancel()
	// Share the quit deadline between audio recovery and simulator cleanup.
	shutdown, stopShutdown := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer stopShutdown()
	s.crewMute.wait(shutdown)
	if s.main != nil {
		select {
		case <-restoreDone:
		case <-shutdown.Done():
		}
		select {
		case <-s.done:
		case <-shutdown.Done():
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
	// Run the first-launch wizard before the main window starts reading preferences.
	backend.RunApp(gui.NewApp(), w)
	return prefs
}

func firstAccentWindow(ctx context.Context, dir, archives string, prefs config.Values, onSaved func(config.Values)) gui.WindowCfg {
	recordings := filepath.Join(dir, "Accents")
	var saved config.Values
	view, busy := accentSetupView(ctx, archives, recordings, prefs, func(enabled []string, fallback string, then func(error)) {
		saved = accentPreferences(prefs.Copy(), enabled, fallback)
		// Write the wizard's selection off the window thread so Continue stays responsive.
		go func() { then(config.Save(dir, saved)) }()
	}, func(w *gui.Window) {
		onSaved(saved)
		w.Close()
	})
	cfg := accentWindow()
	cfg.OnInit = func(w *gui.Window) {
		w.SetView(view)
	}
	cfg.OnCloseRequest = func(w *gui.Window) {
		// Keep the wizard open until the install or settings write finishes
		if !busy() {
			w.Close()
		}
	}
	return cfg
}

// Cancel background work before closing the main window and dropping its tray actions
func (s *state) quit() {
	// The main window and tray must respect the same work as the dialogs
	if s.speechDownloading() || s.dialogsBusy() {
		return
	}
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

func (s *state) mainWindowEvent(e *gui.Event, w *gui.Window) {
	// On Windows the GUI framework reports minimization as occlusion
	if e.Type != gui.EventVisibilityChanged || !w.IsOccluded() {
		return
	}
	// Hide after Windows finishes handling the minimize message
	w.QueueCommand(func(w *gui.Window) {
		if s.tray == nil || !w.IsOccluded() || !w.IsVisible() {
			return
		}
		// Leave dialogs visible if closing them would interrupt their work
		if !s.dialogsBusy() {
			for _, dialog := range s.dialogs {
				dialog.close()
			}
		}
		w.Hide()
	})
}

func (s *state) setLogSecret(key string) {
	if key == "" {
		s.logSecret = nil
	} else {
		// Match the key literally even if it contains regex punctuation or changes case in an error.
		s.logSecret = regexp.MustCompile("(?i)" + regexp.QuoteMeta(key))
	}
}

func (s *state) log(sender, message string) {
	if s.logSecret != nil {
		message = s.logSecret.ReplaceAllString(message, "[redacted]")
	}
	// Keep conversation rows in the window
	for _, line := range strings.Split(strings.TrimSpace(message), "\n") {
		if line != "" {
			s.lines = append(s.lines, fmt.Sprintf("[%s]: %s", sender, line))
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
		s.traceLevel("WARN", "GSX", "Command rejected because the queue is full")
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
		// Wait for telemetry to stop using the transport before closing the session.
		stopPoll()
		if pollDone != nil {
			<-pollDone
		}
	}()
	session.OnReadError = func(err error) {
		s.traceFailure("GSX", "Could not read the panel menu", err)
		s.async(func() { s.log("GSX", "Could not read panel menu") })
	}
	session.OnChoice = func(page gsx.Menu, index int) {
		s.trace("GSX", fmt.Sprintf("Selected slot %d of %d", index+1, len(page.Options)))
		s.menuActivity = time.Now()
		s.async(func() {
			// Only add a highlight if the overlay is still open when the callback runs.
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
	retirePoll := func() {
		select {
		case <-pollDone:
			_ = session.Close()
			pollDone = nil
			s.location.Store(nil)
			lastMenu, lastReopen, s.menuActivity = gsx.Menu{}, time.Time{}, time.Time{}
			s.menuWatch = false
			s.menuRetryAt = time.Time{}
			s.operatorMenu = gsx.Menu{}
			s.operatorSubmitted = time.Time{}
			s.resetServices()
		default:
		}
	}
	for {
		retirePoll()
		select {
		case <-s.ctx.Done():
			return
		case <-pollDone:
			continue
		case job := <-s.jobs:
			// A reconnect can arrive after the outer poll check
			retirePoll()
			ctx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
			if ctx.Err() == nil {
				job(ctx, session)
			}
			cancel()
			// Start telemetry once per connected session after the menu loop has established it.
			if session.Connected() && pollDone == nil {
				pollDone = make(chan struct{})
				go s.pollSimulator(pollCtx, session.Sim, pollDone)
			}
		case <-ticker.C:
			if !session.Connected() {
				s.async(s.retryConnection)
				continue
			}
			retrying := s.retryOverlayMenu(session)
			var menu gsx.Menu
			clearOverlay := false
			if !retrying {
				menu = session.CurrentMenu()
			}
			if session.RemoteActive() {
				// Keep a manual prompt open through transient panel writes
				if len(menu.Options) > 0 && !s.operatorMenu.Same(menu) {
					s.operatorMenu = gsx.Menu{}
					s.operatorSubmitted = time.Time{}
				}
				if len(s.operatorMenu.Options) > 0 {
					// GSX can close the prompt before the panel file changes
					ctx, cancel := context.WithTimeout(s.ctx, time.Second)
					open, err := session.MenuOpened(ctx)
					cancel()
					expired := !s.operatorSubmitted.IsZero() && time.Since(s.operatorSubmitted) >= 4*time.Second
					switch {
					case err == nil && !open, err != nil && expired:
						// A flag read failure must not hold the overlay forever
						s.clearOperatorPrompt(session, err)
						menu, clearOverlay = gsx.Menu{}, true
						retrying = true // Let the scheduled recovery own the next open
					case expired:
						// A prompt that remains open can be tried again by the pilot
						s.operatorSubmitted = time.Time{}
					}
					if !s.operatorSubmitted.IsZero() {
						menu, clearOverlay = gsx.Menu{}, true
					}
				}
				if len(menu.Options) > 0 && !menu.Same(lastMenu) {
					lastMenu, s.menuActivity = menu, time.Now()
				}
				switch {
				case len(s.operatorMenu.Options) == 0 && time.Since(s.menuActivity) >= 10*time.Second:
					s.trace("GSX", "Releasing the idle menu")
					ctx, cancel := context.WithTimeout(s.ctx, 8*time.Second)
					// Save a fresh page before releasing remote control so the overlay still has choices.
					if s.menuWatch && !retrying {
						page, err := session.RefreshAndRelease(ctx)
						menu = page
						clearOverlay = len(page.Options) == 0
						if clearOverlay {
							// Flag-only cleanup can succeed with no rows to display
							s.scheduleOverlayRetry(err)
						}
						if err != nil {
							s.traceFailure("GSX", "Could not refresh and release the idle menu", err)
						}
					} else {
						_ = session.Release(ctx)
					}
					cancel()
				case s.menuWatch && !retrying && len(s.operatorMenu.Options) == 0 && len(menu.Options) == 0 && time.Since(lastReopen) >= 2*time.Second:
					lastReopen = time.Now()
					// A closed menu can require four write checks before GSX publishes a panel.
					ctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
					clearOverlay = s.finishOverlayOpen(session.RetryMenuPage(ctx)) != nil
					cancel()
				}
			}
			if len(menu.Options) > 0 || clearOverlay {
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
	lost := false
	defer func() {
		s.groundSpeed.Store(nil)
		s.pttAltitude.Store(nil)
		if lost {
			s.async(func() {
				s.connected, s.ready, s.overlay = false, false, false
				s.overlayDismissal++
				s.menu, s.highlights = gsx.Menu{}, nil
				s.log("SYS", "Disconnected from SimConnect")
				if s.debugEnabled.Load() {
					go s.traceLocation(s.cfg, nil)
				}
			})
		}
		close(done)
	}()
	session := gsx.New(sim, "")
	ticker := time.NewTicker(600 * time.Millisecond)
	defer ticker.Stop()
	var lastGeoPoll time.Time
	var geoLat, geoLon float64
	var high, fast bool
	var lastReadiness string
	var hadSpeed, speedAvailable bool
	failed := 0
	for ctx.Err() == nil {
		// Let an in-progress status exchange finish during shutdown so it can release the shared transport.
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
		// A missing speed blocks transmission without counting as a lost SimConnect reply.
		if err != nil && !errors.Is(err, gsx.ErrSpeedUnavailable) {
			failed++
		} else {
			failed = 0
		}
		if failed >= 3 || errors.Is(err, gsx.ErrDisconnected) {
			s.traceFailure("SIM", fmt.Sprintf("Connection lost after %d consecutive status failures", max(1, failed)), err)
			lost = true
			return
		}
		// Report changes without logging each telemetry poll
		readiness := "GSX readiness unavailable"
		if ready {
			readiness = "GSX is ready"
		} else if err == nil || errors.Is(err, gsx.ErrSpeedUnavailable) {
			readiness = "Waiting for GSX/Couatl to start"
		}
		if readiness != lastReadiness {
			s.trace("SIM", readiness)
			lastReadiness = readiness
		}
		if available := err == nil; !hadSpeed || available != speedAvailable {
			if available {
				s.trace("SIM", "Ground speed available")
			} else {
				s.traceLevel("WARN", "SIM", "Ground speed unavailable")
			}
			hadSpeed, speedAvailable = true, available
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
			s.pttAltitude.Store(&altitude)
		} else {
			s.pttAltitude.Store(nil)
		}
		// Redraw after both flight readings have been updated
		s.async(func() { s.ready = ready })
		if recovered || descended {
			// The next ground interaction starts with fresh service state and local voices
			s.resetServices()
			lastGeoPoll = time.Time{}
			s.location.Store(nil)
			s.async(func() {
				s.randomizeVoice()
				s.log("SYS", "Ground service state reset. Crew refreshed")
			})
		}
		interval, movement := 5*time.Second, 0.001
		// A local crew changes less often in flight than it does while taxiing
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
					previous := s.location.Swap(&pos)
					if s.debugEnabled.Load() && (previous == nil || *previous != pos) {
						s.async(func() { go s.traceLocation(s.cfg, &pos) })
					}
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

// Manual and automatic attempts share the same cooldown
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
		start := time.Now()
		s.trace("SIM", "Connecting to SimConnect")
		sim, err := gsx.Connect(ctx, func(message string) {
			s.traceLevel("WARN", "GSX", message)
			s.async(func() { s.log("GSX", message) })
		})
		elapsed := time.Since(start).Round(time.Millisecond)
		if err != nil {
			s.traceFailure("SIM", fmt.Sprintf("SimConnect handshake failed after %s", elapsed), err)
			s.async(func() { s.busy = false; s.log("ERR", err.Error()) })
			return
		}
		// A successful SimConnect handshake and GSX startup are separate states
		session.Sim = sim
		s.trace("SIM", fmt.Sprintf("SimConnect handshake completed in %s", elapsed))
		s.async(func() {
			s.busy = false
			s.connected = true
			s.log("SYS", "Connected to SimConnect")
		})
	})
}

// Retry an unavailable panel even after an idle refresh released ownership
func (s *state) retryOverlayMenu(session *gsx.Session) bool {
	if s.menuRetryAt.IsZero() {
		return false
	}
	// The previous request can publish after its wait expires
	if menu := session.TakeRefreshedMenu(); len(menu.Options) > 0 {
		s.publishOverlayMenu(menu)
		return true
	}
	if !time.Now().Before(s.menuRetryAt) {
		// Leave time for four write replies and the first complete panel wait
		ctx, cancel := context.WithTimeout(s.ctx, 7*time.Second)
		defer cancel()
		_ = s.finishOverlayOpen(session.RetryMenuPage(ctx))
	}
	return true
}

// Publish a page only after the session confirms that Couatl refreshed it.
func (s *state) openOverlayMenu(ctx context.Context, session *gsx.Session) error {
	return s.finishOverlayOpen(session.OpenMenuPage(ctx))
}

// Handle overlay recovery consistently for initial and retried opens
func (s *state) finishOverlayOpen(menu gsx.Menu, err error) error {
	s.menuRetryAt = time.Time{}
	if err != nil {
		s.scheduleOverlayRetry(err)
		s.traceFailure("GSX", "Could not open the menu", err)
		return err
	}
	s.publishOverlayMenu(menu)
	return nil
}

// A verified menu ends recovery and replaces any cached overlay rows
func (s *state) publishOverlayMenu(menu gsx.Menu) {
	s.menuRetryAt = time.Time{}
	s.menuActivity = time.Now()
	s.trace("GSX", fmt.Sprintf("Menu opened with %d options", len(menu.Options)))
	s.async(func() {
		if s.overlay {
			s.menu = menu
		}
	})
}

// Missing or locked panels may recover independently of simulator ownership
func (s *state) scheduleOverlayRetry(err error) {
	if s.menuWatch && s.ctx.Err() == nil && !errors.Is(err, gsx.ErrDisconnected) {
		s.menuRetryAt = time.Now().Add(2 * time.Second)
	}
}

func (s *state) toggleMenu() {
	// Leave the overlay visible if the queued close couldn't be scheduled
	if s.overlay {
		if s.addJob(func(ctx context.Context, session *gsx.Session) {
			s.menuWatch = false
			s.menuRetryAt = time.Time{}
			s.operatorMenu = gsx.Menu{}
			s.operatorSubmitted = time.Time{}
			if session.RemoteActive() {
				if err := session.Release(ctx); err != nil {
					s.traceFailure("GSX", "Could not release the menu", err)
				}
			}
		}) {
			s.overlay = false
			s.overlayDismissal++
		}
		return
	}
	if !s.ready {
		return
	}
	if s.addJob(func(ctx context.Context, session *gsx.Session) {
		s.menuWatch = true
		s.menuActivity = time.Now()
		if err := s.openOverlayMenu(ctx, session); err != nil {
			s.async(func() { s.log("GSX", err.Error()) })
		}
	}) {
		s.menu, s.highlights, s.overlay = gsx.Menu{}, nil, true
	}
}

// Discard cached operator rows and release the closed or unresponsive prompt
func (s *state) clearOperatorPrompt(session *gsx.Session, flagErr error) {
	page := s.operatorMenu
	s.operatorMenu, s.operatorSubmitted = gsx.Menu{}, time.Time{}
	s.menuActivity = time.Now()
	session.MarkMenuStale()
	var cleanupErr error
	if flagErr == nil {
		// A new pilot menu can appear before the remote flag is released
		cleanupErr = session.ReleaseClosedMenu(s.ctx)
	} else {
		cleanupErr = session.ReleaseOwned(s.ctx)
	}
	s.scheduleOverlayRetry(errors.Join(flagErr, cleanupErr))
	s.async(func() {
		if s.overlay && s.menu.Same(page) {
			s.menu = gsx.Menu{}
		}
	})
}

// selectNumber runs on the simulator loop so it can reacquire the same page after idle release
func (s *state) selectNumber(ctx context.Context, session *gsx.Session, page gsx.Menu, index int, prefs config.Values) {
	if !s.menuWatch {
		return
	}
	operatorSelection := len(s.operatorMenu.Options) > 0 && s.operatorMenu.Same(page)
	// A queued number may still carry the previously displayed operator snapshot
	if operatorSelection && !s.operatorSubmitted.IsZero() {
		return
	}
	if index < 0 || index >= len(page.Options) {
		s.traceLevel("WARN", "GSX", fmt.Sprintf("Menu slot %d unavailable on a page with %d options", index+1, len(page.Options)))
		s.async(func() {
			s.log("SYS", fmt.Sprintf("GSX menu has no item %d", index+1))
			s.cue(prefs, "error", "", true)
		})
		return
	}
	s.menuActivity = time.Now()
	current := session.CurrentMenu()
	var err error
	if operatorSelection {
		// The old panel can outlive the live menu even between polling ticks
		open, flagErr := session.MenuOpened(ctx)
		err = flagErr
		if err == nil && !open {
			s.clearOperatorPrompt(session, nil)
			current, err = gsx.Menu{}, gsx.ErrMenuChanged
		}
	} else if !s.menuRetryAt.IsZero() {
		// A queued number must wait for the replacement panel to be published
		current, err = gsx.Menu{}, gsx.ErrMenuChanged
	} else if !session.RemoteActive() {
		current, err = session.ReopenMenuPage(ctx, page)
		if errors.Is(err, gsx.ErrMenuChanged) {
			current = session.CurrentMenu()
		}
		// An idle reopen intentionally creates a new snapshot of the displayed page
		if err == nil && page.Same(current) {
			page = current
		}
	}
	if err == nil {
		// Select against the page the user saw because reopening can return different choices.
		err = session.ChooseOn(ctx, page, index)
		if err != nil {
			current = session.CurrentMenu()
		}
	}
	if err != nil {
		s.traceFailure("GSX", fmt.Sprintf("Menu slot %d selection failed", index+1), err)
	}
	operatorPossiblyApplied := operatorSelection && (err == nil || errors.Is(err, gsx.ErrWriteUncertain))
	if operatorPossiblyApplied {
		s.operatorSubmitted = time.Now()
	}
	s.async(func() {
		if operatorPossiblyApplied && s.overlay && s.menu.Same(page) {
			s.menu = gsx.Menu{}
		}
		if err != nil {
			s.log("GSX", err.Error())
			s.cue(prefs, "error", "", true)
			if !operatorPossiblyApplied && s.overlay && (errors.Is(err, gsx.ErrMenuChanged) || len(current.Options) > 0) {
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
	// Publish these settings together so the simulator loop reads one consistent snapshot.
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
	// Treat stale telemetry like a missing reading because the aircraft could have sped up.
	return "Taxi speed unavailable for transmission"
}

func (s *state) loadVocabulary() commands.Vocabulary {
	vocab, warning := s.vocabulary.Load(s.dir)
	// Report a broken vocabulary override while routing continues with the last valid rules.
	if warning != "" {
		s.traceLevel("WARN", "CMD", "Could not load words.json. Using last valid vocabulary")
		s.async(func() {
			s.log("ERR", warning)
			s.main.Toast(gui.ToastCfg{Title: warning})
		})
	}
	return vocab
}

func (s *state) submit(input string) {
	s.submitCommand(input, false)
}

func (s *state) submitCommand(input string, localSpeech bool) {
	input = strings.TrimSpace(input)
	if input == "" {
		return
	}
	// Snapshot window state before handing the request to the simulator loop
	prefs := s.cfg.Copy()
	readoutGeneration := s.readoutGeneration.Load()
	readouts := s.readouts
	page, overlay := s.menu, s.overlay
	dismissal := s.overlayDismissal
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
		text := raw
		// Keep the original transcript available for user-defined codewords
		if localSpeech {
			text = commands.CorrectLocalTranscript(text)
		}
		text = words.Normalize(text)
		// Typed readouts need the simulator and Kokoro, but never require a recognizer
		if readoutMatches(prefs, text, raw) {
			s.trace("CMD", "Routed command to menu readout")
			s.async(func() { s.log("CMD", "GSX menu readout requested") })
			s.sayMenu(ctx, session, prefs, readoutGeneration, readouts, nil)
			return
		}
		gate := func() error {
			policy := s.policy.Load()
			if !session.Connected() && !policy.simulation {
				s.traceLevel("WARN", "CMD", "Command blocked: SimConnect disconnected")
				return gsx.ErrDisconnected
			}
			if blocked := s.speedBlock(policy.simulation, policy.ignoreSpeed); blocked != "" {
				s.traceLevel("WARN", "CMD", "Command blocked: "+blocked)
				return errors.New(blocked)
			}
			if !policy.speechReady {
				s.traceLevel("WARN", "CMD", "Command blocked: speech recognition unavailable or loading")
				return errors.New("Speech recognition is unavailable or still loading")
			}
			return nil
		}
		if err := gate(); err != nil {
			fail(err.Error())
			return
		}
		// Recheck live speed and speech readiness before writing a menu choice.
		session.BeforeChoice = gate
		// Start the mute on every sent selection, including each preset service.
		session.OnSelectionSent = func() {
			_, release := s.muteCrew(s.ctx, prefs, 20*time.Second)
			release()
		}
		manualOperator := false
		session.OnManualOperator = func(page gsx.Menu) {
			// GSX hides its native menu during remote control
			manualOperator = true
			s.menuWatch = true
			s.menuRetryAt = time.Time{}
			s.menuActivity = time.Now()
			s.operatorMenu = page
			s.operatorSubmitted = time.Time{}
			s.async(func() {
				// A Hide click after submission wins over the delayed prompt
				if s.overlayDismissal != dismissal {
					return
				}
				s.menu, s.highlights, s.overlay = page, nil, true
				s.log("GSX", "Select an operator by menu number")
			})
		}
		// Clear callbacks before the session runs another kind of job.
		defer func() {
			session.BeforeChoice = nil
			session.OnSelectionSent = nil
			session.OnManualOperator = nil
		}()
		if idx, ok := commands.Number(text); ok && overlay {
			s.async(func() { s.log("CMD", input) })
			s.selectNumber(ctx, session, page, idx, prefs)
			return
		}
		s.menuActivity = time.Now()
		log := func(sender, msg string) { s.async(func() { s.log(sender, msg) }) }
		action := words.Route(text, raw, config.Get[[]config.Preset](prefs, "presets"), config.Get[bool](prefs, "enable_menu_matching"))
		s.trace("CMD", fmt.Sprintf("Route=%s, services=%d", action.Kind, len(action.Services)))
		explicitIndex, explicitNumber := commands.ExplicitNumber(text)
		// Select a visible slot by number even with Broader Mode disabled
		if overlay && explicitNumber && explicitIndex >= 0 && action.Kind != "preset" {
			log("CMD", input)
			s.selectNumber(ctx, session, page, explicitIndex, prefs)
			return
		}
		// Protect the simulator's operator menu even before the window displays it
		if len(s.operatorMenu.Options) > 0 && (action.Kind == "service" || action.Kind == "preset" || action.Kind == "menu") {
			open, err := session.MenuOpened(ctx)
			if err != nil || open {
				fail("Finish operator selection or hide the GSX menu before starting another command")
				return
			}
			// A command can reach the simulator before the next poll clears the prompt
			s.clearOperatorPrompt(session, nil)
		}
		corrected := false
		// Apply menu correction only after routing known commands
		if action.Kind == "menu" && !explicitNumber && session.Connected() {
			page := session.CurrentMenu()
			opened := len(page.Options) == 0
			if opened {
				page, _ = session.OpenMenuPage(ctx)
			}
			if text2, _ := commands.CorrectMenu(text, page.Options); text2 != "" {
				log("CMD", fmt.Sprintf("%s → %s", text, text2))
				text = text2
				corrected = true
				s.trace("CMD", "Matched command to an option on the current menu")
			}
			if err := gate(); err != nil {
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
		queuedReadouts := 0
		seen := map[string]bool{}
		execute := func(service string) bool {
			// Each preset service gets its own deadline even if earlier ones ran long
			serviceCtx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
			defer cancel()
			if err := gate(); err != nil {
				log("SYS", err.Error())
				return false
			}
			if action.Kind == "preset" && service == config.ReadMenuAction {
				return config.Get[bool](prefs, config.MenuReadoutSetting) && s.sayMenu(serviceCtx, session, prefs, readoutGeneration, readouts, func() {
					queuedReadouts++
				})
			}
			start := time.Now()
			keywords := words.Keywords(service)
			customText, custom := strings.CutPrefix(service, config.CustomServicePrefix)
			// Use the service matcher for literal custom text, including Additional Services
			if custom {
				keywords = nil
				if customText = strings.TrimSpace(customText); customText != "" {
					keywords = []string{customText}
				}
			}
			selected, err := session.Execute(serviceCtx, text, service, keywords, auto)
			if action.Kind == "preset" {
				s.menuActivity = time.Now()
			}
			elapsed := time.Since(start).Round(time.Millisecond)
			if err == nil {
				s.trace("GSX", fmt.Sprintf("Menu command completed in %s", elapsed))
			} else {
				s.traceFailure("GSX", fmt.Sprintf("Menu command failed after %s", elapsed), err)
			}
			if selected != "" {
				matched := service
				if matched == "" || custom {
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
				// A follow-up can fail after the service choice has already taken effect.
				if selected != "" || errors.Is(err, gsx.ErrWriteUncertain) {
					seen[service] = true
				}
				if errors.Is(err, gsx.ErrMenuChanged) {
					page := session.CurrentMenu()
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
			done, readouts := 0, 0
			// Only a definite failure is eligible for a duplicate entry's retry.
			for _, service := range action.Services {
				if s.ctx.Err() != nil {
					return
				}
				if service == config.ReadMenuAction {
					readouts++
				}
				if !seen[service] && execute(service) {
					if service != config.ReadMenuAction {
						done++
					}
					seen[service] = true
				}
				if manualOperator {
					log("PRESET", "Preset stopped for manual operator selection")
					break
				}
			}
			s.async(func() {
				result := fmt.Sprintf("%d/%d services ok", done, len(action.Services)-readouts)
				// Count queued readouts separately from completed GSX services
				if readouts > 0 {
					result += fmt.Sprintf(", %d/%d menu readouts queued", queuedReadouts, readouts)
				}
				s.log("PRESET", fmt.Sprintf("Preset %q complete: %s", action.Preset, result))
				s.trace("CMD", "Preset completed: "+result)
			})
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
	// Check deboarding first because its label also contains boarding.
	for _, service := range []string{"GPU", "JETWAY", "STAIRS", "DEBOARDING", "BOARDING", "CATERING", "CLEANING", "DEICE", "LAVATORY", "PUSHBACK", "REFUELING", "WATER"} {
		if strings.Contains(label, service) {
			return service
		}
	}
	return ""
}
