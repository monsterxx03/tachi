//go:build !headless

package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// The launcher: one system-wide key opens a floating panel, typing filters the installed
// applications, Enter starts one.
//
// It is a second WINDOW of this process rather than a second program, because it needs the things
// only this process has: the host lock, the tray as a way in when the key is taken, and (next) the
// agent itself for the 「问 Tachi」 line. The panel is created HIDDEN at startup and never torn
// down — that is what makes the second press feel instant; a window built on demand pays for its
// webview every time.
const (
	// launcherWindowName is the panel's identity in Wails' window manager, and the name the
	// duplicate-window guard would report.
	launcherWindowName = "launcher"

	// launcherURL distinguishes the panel's page from the main window's: same bundle, one query
	// parameter, and main.tsx renders the launcher instead of the conversation.
	launcherURL = "/?window=launcher"

	launcherWindowWidth = 660
	// launcherWindowHeight is the panel's ceiling: launcherIdleResults rows plus the two fixed bars,
	// i.e. a longer query scrolls this many and no more.
	launcherWindowHeight = 460
	// launcherWindowMinHeight is one input row plus the hint line — what an empty query needs.
	launcherWindowMinHeight = 120
	// launcherTopInset is how far below the screen's work area the panel's top edge sits. It is
	// fixed rather than centred: a panel that grows downward must keep its top still, and the eye
	// looks for it in one place every time.
	launcherTopInset = 96
	// launcherMaxResults bounds what a query can return. The window scrolls; this is only the
	// point past which a list stops helping.
	launcherMaxResults = 200
	// launcherIdleResults is what an EMPTY query shows. Before anything is typed the panel is a
	// short list of what this machine actually launches — not the whole library: a wall of rows
	// says nothing about what to do next, and typing is the way to reach everything else.
	launcherIdleResults = 8
	// launcherScanInterval is the least time between two directory rescans. Every press asks for
	// one (a newly installed app should simply be there), and the interval is what keeps a hand
	// drumming the hotkey from turning into a scan loop.
	launcherScanInterval = 2 * time.Second

	// launcherOpenEventName tells the panel's page it just came up: clear the box, focus it, and
	// re-rank against the empty query. Emitting an unregistered event is silently dropped, so it
	// is registered in main.go's init.
	launcherOpenEventName = "launcher:open"

	// defaultLauncherHotkey matches what the machine's own Spotlight answers before it is turned
	// off — a launcher is remembered by fingers, not by name.
	defaultLauncherHotkey = "Cmd+Space"
	// launcherHotkeyEnv overrides the stored shortcut for one run (dev, and the smoke suite's
	// sandbox, which must not share the reader's key).
	launcherHotkeyEnv = "TACHI_LAUNCHER_HOTKEY"

	launcherCacheFile = "launcher_apps.json"
	launcherUsageFile = "launcher_usage.json"
	// launcherIconDir is where the panel's icons are written, one PNG per bundle. They live under
	// the base directory rather than in memory because the page loads them through the /local
	// asset route — a file it can fetch, not a data URI to carry through every list call.
	launcherIconDir = "launcher_icons"
	// launcherIconPixels is the icon's size on disk. The panel draws it at 28 CSS points, so this
	// is a 2x/3x asset with room to spare, and small enough that a whole library is a few MB.
	launcherIconPixels = 96
	// launcherChangedEventName tells an open panel that the catalog got better — icons drawn, or an
	// application installed/updated/removed since it listed. Registered in main.go's init.
	launcherChangedEventName = "launcher:changed"
)

// LauncherItem is one row of the panel: the name to draw, and the bundle it belongs to. The path is
// the identity — names are not unique (two bundles may both be called 「Xcode」) and a name is what
// the reader can change by renaming a file.
type LauncherItem struct {
	Path string `json:"path"`
	Name string `json:"name"`
	// Detail is the quiet second line: the bundle's location, so two rows with one name can be
	// told apart without opening either.
	Detail string `json:"detail,omitempty"`
	// Icon is the absolute path of this bundle's PNG (see launcherIconDir), or "" while it has not
	// been drawn yet — the panel then shows its letter tile. A path rather than image data: the
	// page loads it through the same /local route every other local picture uses.
	Icon string `json:"icon,omitempty"`
}

// launcherApp is one installed application as the catalog keeps it. The fields without json tags
// are derived on load and never written: the cache stores what a scan cost a subprocess to learn,
// and the pinyin is a dictionary lookup that is cheaper to repeat than to invalidate.
type launcherApp struct {
	Path     string   `json:"path"`
	Name     string   `json:"name"`
	Aliases  []string `json:"aliases,omitempty"`
	BundleID string   `json:"bundleId,omitempty"`
	// Stamp is the modification time of the Info.plist this entry was read from — an app update
	// rewrites it, which is how a cached entry is noticed to be stale.
	Stamp int64 `json:"stamp,omitempty"`
	// Pane marks a row that is a System Settings panel rather than an application. Both are
	// bundles read the same way and ranked the same way; only STARTING one differs (see paneURL).
	Pane bool `json:"pane,omitempty"`

	nameLower      string
	aliasesLower   []string
	pinyinInitials string
	pinyinVariants []string
	words          []string
	wordInitials   string
}

// newLauncherApp reads one bundle off disk and prepares it for matching.
func newLauncherApp(path string) *launcherApp {
	name, aliases, bundleID, stamp := readAppName(path)
	app := &launcherApp{Path: path, Name: name, Aliases: aliases, BundleID: bundleID, Stamp: stamp}
	app.prepare()
	return app
}

// detail is the quiet second line, shown only when two rows share a name: where the bundle sits,
// so 微信 in /Applications can be told from a copy somewhere else. A settings pane gets none — its
// "location" is an .appex directory nobody recognises, and panes are never duplicated anyway.
func (a *launcherApp) detail() string {
	if a.Pane {
		return ""
	}
	return filepath.Dir(a.Path)
}

// prepare fills the derived keys. It is separate from newLauncherApp because a cached entry is
// built from JSON and never touches the disk.
func (a *launcherApp) prepare() {
	a.nameLower = strings.ToLower(a.Name)
	a.aliasesLower = make([]string, 0, len(a.Aliases))
	for _, alias := range a.Aliases {
		a.aliasesLower = append(a.aliasesLower, strings.ToLower(alias))
	}
	forms := pinyinFormsOf(a.Name)
	a.pinyinInitials = forms.initials
	a.pinyinVariants = forms.variants
	a.words = nameWords(a.Name)
	a.wordInitials = wordInitials(a.Name)
}

// launcherUse is one app's history: how often it was started, and when it last was. The pair is
// the whole ranking memory — count alone would keep a one-week fling above a daily tool forever.
type launcherUse struct {
	Count int       `json:"count"`
	Last  time.Time `json:"last"`
}

// score is what a tier tie is broken by. Recency is worth more than a single extra launch, so an
// app used yesterday beats one used five times last year.
func (u launcherUse) score(now time.Time) int {
	score := u.Count * 10
	if u.Last.IsZero() {
		return score
	}
	age := now.Sub(u.Last)
	switch {
	case age < time.Hour:
		score += 60
	case age < 24*time.Hour:
		score += 40
	case age < 7*24*time.Hour:
		score += 20
	case age < 30*24*time.Hour:
		score += 8
	}
	return score
}

type launcherUsage struct {
	Apps map[string]launcherUse `json:"apps"`
}

type launcherCache struct {
	// Version is what the cache was written by. A cache only answers when it matches
	// launcherCacheVersion: the entries are reused by their Info.plist mtime alone, so a change to
	// how a bundle is READ (where its name comes from, which directories are scanned) would
	// otherwise keep answering with the old reading forever — nothing on disk has to move for the
	// catalog to be wrong. Bump the version whenever a scan learns something new about the same
	// bundle.
	Version int            `json:"version"`
	Apps    []*launcherApp `json:"apps"`
}

// launcherCacheVersion is the shape of what a scan produces: bump it whenever a scan learns
// something new about a bundle that is already on disk.
const launcherCacheVersion = 3

// launcher holds the catalog and the panel. It is one value for the process's lifetime; every
// mutation happens under mu, because Wails runs each bound call on its own goroutine.
type launcher struct {
	app    *application.App
	window *application.WebviewWindow

	mu     sync.RWMutex
	apps   []*launcherApp
	usage  map[string]launcherUse
	height int

	// icons maps a bundle path to its drawn PNG. It is separate from the catalog because it is
	// filled LATER — the list must be usable the moment the panel opens, and an icon is a file the
	// system has to rasterise first. Guarded by mu like everything else; a list call copies what is
	// there and rows without an entry draw their letter tile.
	icons map[string]string

	// scanMu guards the rescan's bookkeeping — whether one is running, and when the last one
	// finished. It is NOT mu: a scan holds nothing and must never wait behind a List.
	scanMu    sync.Mutex
	scanning  bool
	scannedAt time.Time

	// hotkey and hotkeyErr are what was asked for and why it did not take. The panel shows the
	// failure: a launcher whose key is owned by Spotlight must say so, not look broken.
	hotkey    string
	hotkeyErr string
}

func newLauncher() *launcher {
	return &launcher{
		usage:  make(map[string]launcherUse),
		icons:  make(map[string]string),
		height: launcherWindowHeight,
	}
}

// install creates the panel and binds the shortcut. It runs once, from main, after the application
// exists (no window can be made before it) and after the theme is resolved.
//
// The panel's palette is the app's DARK one, unconditionally (the boot script in index.html pins
// `data-theme="dark"` for this window): it floats over the desktop instead of sitting inside the
// app's frame, and a light-mode machine would otherwise draw a pale panel that reads as another
// program's window. That is also why the appearance and the pre-paint colour are fixed here rather
// than taken from the reader's theme.
func (l *launcher) install(app *application.App) {
	l.app = app
	l.loadCatalog()
	l.loadUsage()

	window := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:             launcherWindowName,
		Title:            "Tachi 启动器",
		Width:            launcherWindowWidth,
		Height:           launcherWindowHeight,
		MinHeight:        launcherWindowMinHeight,
		Frameless:        true,
		DisableResize:    true,
		AlwaysOnTop:      true,
		Hidden:           true,
		BackgroundType:   application.BackgroundTypeTransparent,
		BackgroundColour: windowBgDark,
		URL:              launcherURL,
		Mac: application.MacWindow{
			TitleBar: application.MacTitleBarHidden,
			// Dark, like the palette: a light system appearance would draw light scrollbars and
			// light focus rings over a dark panel.
			Appearance: application.NSAppearanceNameDarkAqua,
			// A PANEL, not a window (see the option's own doc): a non-activating panel becomes
			// key and takes the keyboard WITHOUT activating this app, so the app the reader was in
			// stays active and gets the focus back the moment the panel hides — no bookkeeping of
			// "who was in front", and the Dock does not flicker on every press.
			WindowClass: application.MacWindowClassPanel,
			PanelPreferences: application.MacPanelPreferences{
				FloatingPanel: true,
				NonActivating: true,
			},
			// It follows the reader across Spaces instead of living on one.
			CollectionBehavior: application.MacWindowCollectionBehaviorCanJoinAllSpaces |
				application.MacWindowCollectionBehaviorFullScreenAuxiliary,
		},
	})
	l.window = window

	// Clicking anywhere else dismisses the panel, which is the same as Esc from the reader's side.
	window.OnWindowEvent(events.Common.WindowLostFocus, func(*application.WindowEvent) {
		window.Hide()
	})

	l.registerHotkey()
	l.refreshAsync()
}

// registerHotkey binds the system-wide key. A failure is NOT fatal: the tray item is the other way
// in, and the panel says what went wrong.
func (l *launcher) registerHotkey() {
	hotkey := launcherHotkey()
	l.hotkey = hotkey
	if demoEnabled() {
		// A scripted run opens a window on someone's machine: it must never also take a
		// system-wide keystroke that belongs to them.
		l.hotkey = ""
		return
	}
	if err := l.app.GlobalShortcut.Register(hotkey, func() { l.toggle() }); err != nil {
		l.hotkeyErr = err.Error()
		// Logged through the app's own logger, not the standard one: stderr of a login-item launch
		// goes to /dev/null, so a standard-library line here is a line nobody can ever read.
		logger.New("desktop").Warn(context.Background(),
			"launcher: the system-wide key is unavailable — the tray's 「启动器」 is the way in",
			"hotkey", hotkey, "err", err.Error())
	}
}

// toggle is the hotkey's action: show when hidden, hide when shown.
func (l *launcher) toggle() {
	if l.window == nil {
		return
	}
	if l.window.IsVisible() {
		l.hide()
		return
	}
	l.show()
}

// show places and reveals the panel, then tells its page to start over.
func (l *launcher) show() {
	if l.window == nil {
		return
	}
	l.place()
	l.window.Show()
	l.window.Focus()
	l.app.Event.Emit(launcherOpenEventName, "")
	// An app installed since the last press should simply be there. The scan is cheap enough to
	// ask for every time (see launcherScanInterval) and runs in the background.
	l.refreshAsync()
}

func (l *launcher) hide() {
	if l.window != nil {
		l.window.Hide()
	}
}

// place puts the panel where it is looked for: centred on the screen the POINTER is on, its top edge
// a fixed distance down. Called on every show, because both the screen a person is on and the number
// of screens can change between two presses.
//
// Following the pointer rather than the main window is the whole point: the reader presses the key
// while looking at the screen they are working on, and a panel that opens somewhere else costs them
// a head turn — the same reason Spotlight appears where the mouse is.
func (l *launcher) place() {
	width, height := launcherWindowWidth, l.currentHeight()
	area := l.targetWorkArea()
	x := area.X + (area.Width-width)/2
	y := area.Y + launcherTopInset

	// A last guard, whatever the numbers above came from: a panel that is not on a display is a
	// panel the reader cannot see. If the rectangle lands outside every attached screen, use the
	// first one and say so — that is the fingerprint of a screen list gone stale.
	if screens := launcherScreens(); len(screens) > 0 && !visibleOnAnyScreen(screens, x, y, width, height) {
		logger.New("desktop").Warn(context.Background(),
			"launcher: placement landed off every display; using the first screen's work area",
			"x", x, "y", y, "area", fmt.Sprintf("%+v", area),
			"screen", fmt.Sprintf("%+v", screens[0]))
		first := screens[0].Work
		x = first.X + (first.Width-width)/2
		y = first.Y + launcherTopInset
	}

	l.window.SetSize(width, height)
	l.window.SetPosition(x, y)
}

// launcherArea is a rectangle in the point space the panel is placed in: origin at the TOP-LEFT of
// the main display.
type launcherArea struct {
	X, Y, Width, Height int
}

// launcherScreenArea is one attached display: where it is (Bounds) and where a window may go on it
// (Work — the screen minus the menu bar and the Dock).
type launcherScreenArea struct {
	Bounds launcherArea
	Work   launcherArea
}

func (a launcherArea) contains(x, y int) bool {
	return x >= a.X && x < a.X+a.Width && y >= a.Y && y < a.Y+a.Height
}

// visibleOnAnyScreen asks whether a rectangle's middle lands on some display.
func visibleOnAnyScreen(screens []launcherScreenArea, x, y, width, height int) bool {
	for _, screen := range screens {
		if screen.Bounds.contains(x+width/2, y+height/2) {
			return true
		}
	}
	return false
}

// targetWorkArea is the work area of the screen under the pointer, or the first screen's when the
// platform cannot say where the pointer is.
//
// The displays come from AppKit directly (launcherScreens) rather than from the window manager's
// list. That list is refreshed on display-change events, and one missed event leaves it holding the
// geometry of a display that is no longer attached — which is how the panel once opened thousands
// of points off screen when an external monitor was the only one left. The window manager's list
// stays as the fallback for platforms that have no answer of their own.
func (l *launcher) targetWorkArea() launcherArea {
	if screens := launcherScreens(); len(screens) > 0 {
		if px, py, ok := launcherCursor(); ok {
			for _, screen := range screens {
				if screen.Bounds.contains(px, py) {
					return screen.Work
				}
			}
		}
		return screens[0].Work
	}

	primary := launcherArea{}
	if screen := l.app.Screen.GetPrimary(); screen != nil {
		primary = launcherArea{X: screen.WorkArea.X, Y: screen.WorkArea.Y, Width: screen.WorkArea.Width, Height: screen.WorkArea.Height}
	}
	px, py, ok := launcherCursor()
	if !ok {
		return primary
	}
	for _, screen := range l.app.Screen.GetAll() {
		if screen == nil {
			continue
		}
		b := launcherArea{X: screen.Bounds.X, Y: screen.Bounds.Y, Width: screen.Bounds.Width, Height: screen.Bounds.Height}
		if b.contains(px, py) {
			return launcherArea{X: screen.WorkArea.X, Y: screen.WorkArea.Y, Width: screen.WorkArea.Width, Height: screen.WorkArea.Height}
		}
	}
	return primary
}

func (l *launcher) currentHeight() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.height
}

// resize fits the panel to the rows there are, within [min, ceiling]. The frontend asks after every
// list, which is what keeps an empty query from opening a tall panel of nothing.
func (l *launcher) resize(height int) {
	if height < launcherWindowMinHeight {
		height = launcherWindowMinHeight
	}
	if height > launcherWindowHeight {
		height = launcherWindowHeight
	}
	l.mu.Lock()
	l.height = height
	l.mu.Unlock()
	if l.window != nil {
		l.window.SetSize(launcherWindowWidth, height)
	}
}

// list ranks the catalog against a query. An empty query answers the whole catalog in usage order
// — what a launcher shows before anything is typed.
func (l *launcher) list(query string) []LauncherItem {
	q := normalizeQuery(query)
	now := time.Now()

	l.mu.RLock()
	apps := l.apps
	icons := l.icons
	scores := make(map[string]int, len(l.usage))
	for path, use := range l.usage {
		scores[path] = use.score(now)
	}
	l.mu.RUnlock()

	type hit struct {
		app  *launcherApp
		tier int
		use  int
	}
	hits := make([]hit, 0, len(apps))
	for _, app := range apps {
		if tier, ok := app.matchTier(q); ok {
			hits = append(hits, hit{app: app, tier: tier, use: scores[app.Path]})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].tier != hits[j].tier {
			return hits[i].tier > hits[j].tier
		}
		if hits[i].use != hits[j].use {
			return hits[i].use > hits[j].use
		}
		return hits[i].app.Name < hits[j].app.Name
	})
	limit := launcherMaxResults
	if q == "" {
		// Nothing typed yet: what this machine actually launches, and no more.
		limit = launcherIdleResults
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}

	items := make([]LauncherItem, len(hits))
	for i, h := range hits {
		items[i] = LauncherItem{
			Path:   h.app.Path,
			Name:   h.app.Name,
			Detail: h.app.detail(),
			Icon:   icons[h.app.Path],
		}
	}
	return items
}

// launch starts an app (or opens a settings pane) and then closes the panel.
//
// The order is launch-then-hide, and both halves are deliberate:
//   - the bundle is checked for BEFORE anything happens, because an app that has been uninstalled
//     still sits in the cache until the next scan — pressing Enter on it would otherwise hide the
//     panel and leave the reader staring at nothing, unable to tell a slow launch from a dead row;
//   - the panel closes only after the launch succeeded, so a failure can still be said out loud in
//     the one place the reader is looking. A panel that hides first has already spent its voice.
func (l *launcher) launch(path string) error {
	app := l.knownApp(path)
	name := filepath.Base(path)
	if app != nil {
		name = app.Name
	}
	if _, err := os.Stat(path); err != nil {
		l.forget(path)
		return fmt.Errorf("「%s」找不到了，可能已经卸载或移动", name)
	}
	// A settings pane has a bundle on disk but is started by URL, not by its bundle: `open -a` on
	// a .appex starts nothing, because an extension has no life of its own to start.
	var err error
	if app != nil && app.Pane {
		err = openPane(app.paneURL())
	} else {
		err = openApp(path)
	}
	if err != nil {
		return fmt.Errorf("「%s」没能启动：%w", name, err)
	}
	l.hide()
	l.recordUse(path)
	return nil
}

// knownName is the row's title, for a message about it after it is gone.
func (l *launcher) knownName(path string) string {
	if app := l.knownApp(path); app != nil {
		return app.Name
	}
	return filepath.Base(path)
}

// knownApp is the catalog entry for a path, or nil when the row is gone.
func (l *launcher) knownApp(path string) *launcherApp {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, app := range l.apps {
		if app.Path == path {
			return app
		}
	}
	return nil
}

// forget drops a bundle from the catalog because it is no longer there.
//
// Both containers are REPLACED, never edited: a List call hands the slice and the icon map it read
// out to code that runs outside the lock, so deleting a row in place would either hand it a
// half-built slice or, for the map, race with its reads outright. A fresh map per forgotten app is
// a few hundred entries — cheaper than the bug.
func (l *launcher) forget(path string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := make([]*launcherApp, 0, len(l.apps))
	for _, app := range l.apps {
		if app.Path != path {
			kept = append(kept, app)
		}
	}
	l.apps = kept
	icons := make(map[string]string, len(l.icons))
	for p, icon := range l.icons {
		if p != path {
			icons[p] = icon
		}
	}
	l.icons = icons
}

// recordUse bumps one app's history and writes it. It is written on every launch rather than at
// exit: a launcher's ranking is the point, and a crash must not lose the day's evidence.
func (l *launcher) recordUse(path string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	use := l.usage[path]
	use.Count++
	use.Last = time.Now()
	l.usage[path] = use
	if err := fileutil.AtomicWriteJSONPrivate(launcherUsagePath(), launcherUsage{Apps: l.usage}); err != nil {
		log.Printf("launcher: recording usage failed: %v", err)
	}
}

// refreshAsync brings the catalog up to date in the background: what is installed now, and the
// icons that go with it.
//
// It is asked for on every press (and once at startup) and answers immediately — the panel is drawn
// from whatever the catalog says right now. A directory scan is cheap (five ReadDir calls; a plutil
// only runs for a bundle whose Info.plist changed) but it is not instant, and no one is going to
// wait for it before typing.
//
// Scanning is serialised by scanMu and rate-limited by launcherScanInterval, which is what keeps a
// hand drumming the hotkey from becoming a scan loop.
func (l *launcher) refreshAsync() {
	l.scanMu.Lock()
	if l.scanning || time.Since(l.scannedAt) < launcherScanInterval {
		l.scanMu.Unlock()
		return
	}
	l.scanning = true
	l.scanMu.Unlock()

	go func() {
		changed := l.refreshCatalog()
		drew := l.generateIcons()
		l.scanMu.Lock()
		l.scanning = false
		l.scannedAt = time.Now()
		l.scanMu.Unlock()
		// Told AFTER both halves are in place, so an open panel re-lists into a finished state
		// rather than a half-drawn one — and only when there is something new to show.
		if (changed || drew) && l.app != nil {
			l.app.Event.Emit(launcherChangedEventName, "")
		}
	}()
}

// loadCatalog answers from the cache written by the previous run, so the first press after a launch
// already has something to show. refreshCatalog replaces it with what is installed now.
func (l *launcher) loadCatalog() {
	var cache launcherCache
	if err := fileutil.ReadJSON(launcherCachePath(), &cache); err != nil {
		return
	}
	if cache.Version != launcherCacheVersion {
		// Written by a scan that read bundles a different way. The entries would answer with what
		// that scan knew (an English name for 访达), so they are dropped and the rescan on the
		// first press refills the catalog.
		return
	}
	apps := make([]*launcherApp, 0, len(cache.Apps))
	for _, app := range cache.Apps {
		if app == nil || app.Path == "" {
			continue
		}
		app.prepare()
		apps = append(apps, app)
	}
	l.replaceCatalog(apps)
}

// refreshCatalog rescans the application directories, re-reads only the bundles whose Info.plist
// changed, and adds the System Settings panes — then writes the cache back. It answers whether
// anything changed: a rescan that found the same bundles must not cost a cache write or a re-list
// of an open panel.
func (l *launcher) refreshCatalog() bool {
	paths := scanAppBundles()
	if len(paths) == 0 {
		return false
	}

	previous := make(map[string]*launcherApp)
	l.mu.RLock()
	for _, app := range l.apps {
		previous[app.Path] = app
	}
	l.mu.RUnlock()

	// A bundle is re-read only when the Info.plist it was read from has changed since. An app that
	// merely appears keeps nothing of the previous scan, so the second launch of a new app is
	// already cache-only.
	apps := make([]*launcherApp, len(paths))
	fresh := make([]string, 0, len(paths))
	freshAt := make([]int, 0, len(paths))
	for i, path := range paths {
		cached, ok := previous[path]
		if ok {
			if info, err := os.Stat(filepath.Join(bundleRoot(path), "Info.plist")); err == nil &&
				cached.Stamp == info.ModTime().UnixNano() && !cached.Pane {
				apps[i] = cached
				continue
			}
		}
		fresh = append(fresh, path)
		freshAt = append(freshAt, i)
	}
	for i, app := range readApps(fresh) {
		apps[freshAt[i]] = app
	}

	// Panes are appended after the applications rather than mixed in. Order only matters before
	// anything is typed (the idle list is by usage, then name), and keeping them in one block is
	// what lets the sidebar's own order come through instead of being sorted away.
	apps = append(apps, panesOf()...)

	// A bundle that appeared, was updated or went away — the three things a reader would notice.
	changed := len(fresh) > 0 || len(previous) != len(apps)
	l.replaceCatalog(apps)
	if changed {
		l.saveCatalog(apps)
		log.Printf("launcher: %d rows (%d applications re-read)", len(apps), len(fresh))
	}
	return changed
}

func (l *launcher) replaceCatalog(apps []*launcherApp) {
	sort.SliceStable(apps, func(i, j int) bool {
		if apps[i].Name != apps[j].Name {
			return apps[i].Name < apps[j].Name
		}
		return apps[i].Path < apps[j].Path
	})
	l.mu.Lock()
	l.apps = apps
	l.mu.Unlock()
}

func (l *launcher) saveCatalog(apps []*launcherApp) {
	if err := fileutil.AtomicWriteJSONPrivate(launcherCachePath(), launcherCache{Version: launcherCacheVersion, Apps: apps}); err != nil {
		log.Printf("launcher: writing the application cache failed: %v", err)
	}
}

func (l *launcher) loadUsage() {
	var usage launcherUsage
	if err := fileutil.ReadJSON(launcherUsagePath(), &usage); err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for path, use := range usage.Apps {
		l.usage[path] = use
	}
}

// launcherHotkey is the shortcut this run will try to bind: the environment's, else the stored
// preference, else the default.
func launcherHotkey() string {
	if fromEnv := strings.TrimSpace(os.Getenv(launcherHotkeyEnv)); fromEnv != "" {
		return fromEnv
	}
	if stored := strings.TrimSpace(loadUIState().LauncherHotkey); stored != "" {
		return stored
	}
	return defaultLauncherHotkey
}

func launcherCachePath() string   { return filepath.Join(config.BaseDir(), launcherCacheFile) }
func launcherUsagePath() string   { return filepath.Join(config.BaseDir(), launcherUsageFile) }
func launcherIconDirPath() string { return filepath.Join(config.BaseDir(), launcherIconDir) }

// launcherIconPath is where one bundle's icon lives: the digest of its path, so the same app keeps
// the same file across runs and two bundles with one name never collide.
func launcherIconPath(appPath string) string {
	sum := sha1.Sum([]byte(appPath))
	return filepath.Join(launcherIconDirPath(), hex.EncodeToString(sum[:])+".png")
}

// generateIcons draws the icons the panel does not have yet.
//
// It runs AFTER the catalog, on its own goroutine, because the two have different costs: the list
// is usable as soon as the scan finishes (with letter tiles), while an icon means asking the system
// to rasterise an image and writing a file. Making the panel wait for that would put a second of
// nothing in front of every first press.
//
// An icon is stale when the bundle that produced it has been updated — the same Info.plist mtime the
// catalog itself is cached against. It answers whether anything was drawn, which is what decides
// whether an open panel is worth re-listing.
func (l *launcher) generateIcons() bool {
	dir := launcherIconDirPath()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("launcher: creating the icon directory failed: %v", err)
		return false
	}

	l.mu.RLock()
	apps := append([]*launcherApp(nil), l.apps...)
	l.mu.RUnlock()

	icons := make(map[string]string, len(apps))
	keep := make(map[string]bool, len(apps))
	failed, drawn := 0, 0
	for _, app := range apps {
		path := launcherIconPath(app.Path)
		keep[path] = true
		if !iconFresh(path, app.Stamp) {
			if err := writeAppIcon(app.Path, path, launcherIconPixels); err != nil {
				// A bundle whose icon cannot be drawn is not an error worth stopping for: that row
				// keeps its letter tile.
				failed++
				continue
			}
			drawn++
		}
		icons[app.Path] = path
	}

	l.mu.Lock()
	l.icons = icons
	l.mu.Unlock()

	pruneIcons(dir, keep)
	if failed > 0 {
		log.Printf("launcher: %d of %d icons could not be drawn", failed, len(apps))
	}
	return drawn > 0
}

// iconFresh reports whether the PNG on disk still belongs to this version of the bundle.
func iconFresh(iconPath string, stamp int64) bool {
	info, err := os.Stat(iconPath)
	if err != nil {
		return false
	}
	return info.ModTime().UnixNano() >= stamp
}

// pruneIcons removes the PNGs of apps that are gone. Without it an uninstalled app would leave its
// picture in the base directory for good — a cache nobody ever reads again.
func pruneIcons(dir string, keep map[string]bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".png") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if keep[path] {
			continue
		}
		if err := os.Remove(path); err != nil {
			log.Printf("launcher: removing a stale icon failed: %v", err)
		}
	}
}

// LauncherService is the panel's page's way into this process. It exists separately from
// AgentService because the launcher is not the conversation: nothing here touches a session, and a
// reader closing the panel must not disturb one.
type LauncherService struct {
	launcher *launcher
}

// LauncherHotkeyVO is what the panel needs to explain itself when the key did not take.
type LauncherHotkeyVO struct {
	Shortcut string `json:"shortcut"`
	Error    string `json:"error,omitempty"`
}

// List answers the rows for a query (see launcher.list).
func (s *LauncherService) List(query string) []LauncherItem {
	return s.launcher.list(query)
}

// Launch starts an app and closes the panel.
func (s *LauncherService) Launch(path string) error {
	return s.launcher.launch(path)
}

// Hide closes the panel — what Esc calls.
func (s *LauncherService) Hide() {
	s.launcher.hide()
}

// Resize fits the panel to the rows the page is drawing.
func (s *LauncherService) Resize(height int) {
	s.launcher.resize(height)
}

// Hotkey reports what was bound, so a key somebody else owns is visible instead of looking broken.
func (s *LauncherService) Hotkey() LauncherHotkeyVO {
	return LauncherHotkeyVO{Shortcut: s.launcher.hotkey, Error: s.launcher.hotkeyErr}
}
