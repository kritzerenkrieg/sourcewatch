package poller

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const gtAddr = "46.105.73.18:27015"

var gtNow = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

func fixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/gametracker_alive.html")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseGameTrackerAlive(t *testing.T) {
	res, err := ParseGameTracker([]byte(fixture(t)), gtAddr, gtNow)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Alive {
		t.Fatal("Status: Alive should be alive")
	}
	if res.Age != 6*time.Minute+59*time.Second || !res.ScannedAt.Equal(gtNow.Add(-res.Age)) {
		t.Errorf("age %v, scanned %v", res.Age, res.ScannedAt)
	}
	st := res.State
	if st.Name != "Zombie Escape ● NiDE.GG ● NoSteam | discord.gg/nide" || st.Map != "ze_bhop_facility_final_v2" || st.Players != 8 || st.MaxPlayers != 65 {
		t.Errorf("state %+v", st)
	}
	want := []string{"Neymar Jr.", "malup", "NiDE TV", "Pivovaric", "Kritz", "Pyroman", "S T F V", "Oppenheimer"}
	if !st.NamesKnown || !reflect.DeepEqual(st.PlayerNames, want) {
		t.Errorf("names %v (known %v), want %v", st.PlayerNames, st.NamesKnown, want)
	}
}

func TestParseGameTrackerNotAliveMeansOffline(t *testing.T) {
	page := fixture(t)
	for name, mod := range map[string]string{
		"other status":   strings.Replace(page, ">\n\t\t\t\tAlive\n\t\t\t<", ">Dead<", 1),
		"status removed": removeStatus(page),
		"empty status":   strings.Replace(page, "Alive", "", 1),
	} {
		if mod == page {
			t.Fatalf("%s: the fixture was not changed", name)
		}
		res, err := ParseGameTracker([]byte(mod), gtAddr, gtNow)
		if err != nil || res.Alive {
			t.Errorf("%s: alive=%v err=%v, want offline without error", name, res.Alive, err)
		}
	}
}

// removeStatus cuts the whole Status field out of the page.
func removeStatus(page string) string {
	loc := reStatus.FindStringIndex(page)
	if loc == nil {
		return page
	}
	return page[:loc[0]] + page[loc[1]:]
}

func TestParseGameTrackerRejectsWhatItDoesNotUnderstand(t *testing.T) {
	page := fixture(t)
	for name, mod := range map[string]string{
		"garbage":         "<html>nothing here</html>",
		"empty":           "",
		"no scan time":    strings.Replace(page, "Last scanned", "Checked", 1),
		"bad scan time":   strings.Replace(page, "6 minutes 59 seconds", "a while", 1),
		"other server":    strings.ReplaceAll(page, "46.105.73.18", "1.2.3.4"),
		"no address":      strings.Replace(page, "Address:", "Where:", 1),
		"no map":          strings.Replace(page, "HTML_curr_map", "x", 1),
		"no player count": strings.Replace(page, "HTML_num_players", "x", 1),
		"no name":         strings.Replace(page, "Name:", "Title:", 1),
	} {
		_, err := ParseGameTracker([]byte(mod), gtAddr, gtNow)
		if !errors.Is(err, ErrNotUnderstood) {
			t.Errorf("%s: err %v, want ErrNotUnderstood", name, err)
		}
	}
}

func TestParseGameTrackerDetails(t *testing.T) {
	page := fixture(t)
	// HTML entities in the name are decoded.
	res, _ := ParseGameTracker([]byte(strings.Replace(page, "Zombie Escape", "Zombie &amp; Escape", 1)), gtAddr, gtNow)
	if !strings.HasPrefix(res.State.Name, "Zombie & Escape") {
		t.Errorf("name %q", res.State.Name)
	}
	// Bots are not counted, like with A2S.
	withBots := strings.Replace(page, "<div>", `<span id="HTML_num_bots">3</span><div>`, 1)
	res, _ = ParseGameTracker([]byte(withBots), gtAddr, gtNow)
	if res.State.Players != 5 {
		t.Errorf("players %d, want 8-3", res.State.Players)
	}
	// No player table: names unknown unless the server is empty.
	noTable := page[:strings.Index(page, "ONLINE PLAYERS")]
	res, _ = ParseGameTracker([]byte(noTable), gtAddr, gtNow)
	if res.State.NamesKnown || len(res.State.PlayerNames) != 0 {
		t.Errorf("names should be unknown without the table: %+v", res.State)
	}
	empty := strings.Replace(noTable, `">8</span>`, `">0</span>`, 1)
	res, _ = ParseGameTracker([]byte(empty), gtAddr, gtNow)
	if !res.State.NamesKnown {
		t.Errorf("an empty server has no names to be missing: %+v", res.State)
	}
}

func TestParseAge(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"8 minutes 41 seconds":       8*time.Minute + 41*time.Second,
		"45 seconds":                 45 * time.Second,
		"1 minute 1 second":          time.Minute + time.Second,
		"1 hour 2 minutes 3 seconds": time.Hour + 2*time.Minute + 3*time.Second,
		"2 days 1 hour":              49 * time.Hour,
	} {
		if got, ok := parseAge(in); !ok || got != want {
			t.Errorf("%q: %v %v, want %v", in, got, ok, want)
		}
	}
	if _, ok := parseAge("a while"); ok {
		t.Error("garbage should not parse")
	}
}

func TestFetch(t *testing.T) {
	var gotPath, gotUA atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.Path)
		gotUA.Store(r.Header.Get("User-Agent"))
		if strings.Contains(r.URL.Path, "9.9.9.9") {
			http.Error(w, "nope", http.StatusForbidden)
			return
		}
		w.Write([]byte(fixture(t)))
	}))
	defer srv.Close()
	g := &GameTracker{Client: srv.Client(), BaseURL: srv.URL, LookupIP: func(string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("46.105.73.18")}, nil
	}}

	// A host name is resolved to the IP the page is addressed by.
	res, err := g.Fetch(context.Background(), "css.example:27015", gtNow)
	if err != nil || !res.Alive {
		t.Fatalf("%v %v", res, err)
	}
	if gotPath.Load() != "/server_info/46.105.73.18:27015/" {
		t.Errorf("path %v", gotPath.Load())
	}
	if ua, _ := gotUA.Load().(string); !strings.HasPrefix(ua, "sourcewatch/") {
		t.Errorf("user agent %q should name sourcewatch", ua)
	}
	// An IP literal is not looked up.
	g.LookupIP = func(string) ([]net.IP, error) { t.Fatal("lookup for an IP literal"); return nil, nil }
	if _, err := g.Fetch(context.Background(), gtAddr, gtNow); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Fetch(context.Background(), "9.9.9.9:27015", gtNow); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("non-200 should be an error: %v", err)
	}
	g.LookupIP = func(string) ([]net.IP, error) { return nil, errors.New("no such host") }
	if _, err := g.Fetch(context.Background(), "nx.example:27015", gtNow); err == nil {
		t.Error("unresolvable host should be an error")
	}
	if _, err := g.Fetch(context.Background(), "no-port", gtNow); err == nil {
		t.Error("an address without a port should be an error")
	}
}

// --- poller integration ---

type fakeFallback struct {
	mu    sync.Mutex
	calls int
	res   GTResult
	err   error
}

func (f *fakeFallback) Fetch(context.Context, string, time.Time) (GTResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.res, f.err
}

func (f *fakeFallback) set(res GTResult, err error) {
	f.mu.Lock()
	f.res, f.err = res, err
	f.mu.Unlock()
}

func (f *fakeFallback) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func alive(name string, scanned time.Time, age time.Duration, players ...string) GTResult {
	return GTResult{Alive: true, Age: age, ScannedAt: scanned, State: State{Name: name, Map: "m", Players: len(players), PlayerNames: players, NamesKnown: true}}
}

// a2sDown is a Dialer whose server never answers.
func a2sDown(string) (Client, error) { return nil, errors.New("timeout") }

func fallbackPoller(col *collector, fb Fallback, clk *clock) *Poller {
	return &Poller{Dial: a2sDown, Interval: 4 * time.Millisecond, OnUpdate: col.add, Fallback: fb, Now: clk.now,
		FallbackEvery: 5 * time.Minute, MaxDataAge: 20 * time.Minute}
}

func run(t *testing.T, p *Poller) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	m := NewManager(ctx, p)
	m.Watch("a:1")
	return func() { cancel(); m.Wait() }
}

func TestFallbackReplacesOfflineAfterThreeFailures(t *testing.T) {
	var col collector
	clk := &clock{t: gtNow}
	fb := &fakeFallback{}
	fb.set(alive("Zombie", gtNow.Add(-3*time.Minute), 3*time.Minute, "alice"), nil)
	defer run(t, fallbackPoller(&col, fb, clk))()

	waitFor(t, func() bool {
		for _, u := range col.get("a:1") {
			if u.Source == SourceGameTracker {
				return true
			}
		}
		return false
	})
	us := col.get("a:1")
	for _, u := range us {
		if u.Transition && !u.Online {
			t.Fatalf("declared offline although the fallback said alive: %+v", u)
		}
	}
	var gt Update
	for _, u := range us {
		if u.Source == SourceGameTracker {
			gt = u
			break
		}
	}
	if !gt.Online || !gt.Transition || gt.Err != nil || gt.State.Name != "Zombie" || !gt.DataAt.Equal(gtNow.Add(-3*time.Minute)) {
		t.Fatalf("fallback update %+v", gt)
	}
	// Before the third failure there was no update that claimed online.
	if first := us[0]; first.Online || first.Transition {
		t.Fatalf("first failed cycle %+v", first)
	}
}

func TestFallbackFetchesRarelyAndAppliesOnlyNewScans(t *testing.T) {
	var col collector
	clk := &clock{t: gtNow}
	fb := &fakeFallback{}
	scan1 := gtNow.Add(-2 * time.Minute)
	fb.set(alive("Zombie", scan1, 2*time.Minute, "alice"), nil)
	defer run(t, fallbackPoller(&col, fb, clk))()

	gtUpdates := func() (n int) {
		for _, u := range col.get("a:1") {
			if u.Source == SourceGameTracker {
				n++
			}
		}
		return
	}
	waitFor(t, func() bool { return gtUpdates() == 1 })
	time.Sleep(60 * time.Millisecond) // many poll cycles, no time passes for the fallback
	if fb.count() != 1 || gtUpdates() != 1 {
		t.Fatalf("fetches %d, updates %d: want 1 and 1 within the interval", fb.count(), gtUpdates())
	}

	// Interval passes, but GameTracker has not scanned again: same scan, no update.
	clk.advance(6 * time.Minute)
	fb.set(alive("Zombie", scan1, 8*time.Minute, "alice"), nil)
	waitFor(t, func() bool { return fb.count() == 2 })
	time.Sleep(40 * time.Millisecond)
	if gtUpdates() != 1 {
		t.Fatalf("applied the same scan again: %d updates", gtUpdates())
	}

	// A new scan is applied, with the new data.
	clk.advance(6 * time.Minute)
	fb.set(alive("Zombie", clk.now().Add(-time.Minute), time.Minute, "alice", "bob"), nil)
	waitFor(t, func() bool { return gtUpdates() == 2 })
	last := col.get("a:1")
	if got := last[len(last)-1]; got.State.Players != 2 || got.Transition {
		t.Fatalf("new scan update %+v", got)
	}
}

func TestFallbackNotAliveStaleOrBrokenMeansOffline(t *testing.T) {
	for name, tc := range map[string]struct {
		res GTResult
		err error
	}{
		"status not alive": {res: GTResult{Alive: false, Age: time.Minute, ScannedAt: gtNow}},
		"stale scan":       {res: alive("Z", gtNow.Add(-30*time.Minute), 30*time.Minute)},
		"fetch error":      {err: errors.New("HTTP 503")},
		"page not parsed":  {err: ErrNotUnderstood},
	} {
		t.Run(name, func(t *testing.T) {
			var col collector
			clk := &clock{t: gtNow}
			fb := &fakeFallback{}
			fb.set(tc.res, tc.err)
			defer run(t, fallbackPoller(&col, fb, clk))()
			waitFor(t, func() bool {
				for _, u := range col.get("a:1") {
					if u.Transition && !u.Online {
						return true
					}
				}
				return false
			})
			for _, u := range col.get("a:1") {
				if u.Online || u.Source == SourceGameTracker {
					t.Fatalf("used unusable fallback data: %+v", u)
				}
			}
		})
	}
}

func TestFallbackGoesOfflineWhenGameTrackerStops(t *testing.T) {
	var col collector
	clk := &clock{t: gtNow}
	fb := &fakeFallback{}
	fb.set(alive("Zombie", gtNow.Add(-time.Minute), time.Minute, "alice"), nil)
	defer run(t, fallbackPoller(&col, fb, clk))()
	waitFor(t, func() bool { return len(col.get("a:1")) >= 3 && col.get("a:1")[2].Online })

	// GameTracker stops scanning: the next fetch shows an old scan.
	clk.advance(6 * time.Minute)
	fb.set(alive("Zombie", gtNow.Add(-time.Minute), 25*time.Minute, "alice"), nil)
	waitFor(t, func() bool {
		us := col.get("a:1")
		return !us[len(us)-1].Online
	})
	us := col.get("a:1")
	if last := us[len(us)-1]; !last.Transition || last.Err == nil {
		t.Fatalf("last update %+v", last)
	}
}

func TestA2SRecoveryStopsUsingFallback(t *testing.T) {
	var col collector
	clk := &clock{t: gtNow}
	fb := &fakeFallback{}
	fb.set(alive("Zombie", gtNow.Add(-time.Minute), time.Minute, "alice"), nil)
	var up atomic.Bool
	p := fallbackPoller(&col, fb, clk)
	p.Dial = func(string) (Client, error) {
		if up.Load() {
			return &fake{info: Info{Name: "Real", Map: "m", Players: 1, MaxPlayers: 8}}, nil
		}
		return nil, errors.New("timeout")
	}
	defer run(t, p)()
	waitFor(t, func() bool {
		for _, u := range col.get("a:1") {
			if u.Source == SourceGameTracker {
				return true
			}
		}
		return false
	})
	up.Store(true)
	waitFor(t, func() bool {
		us := col.get("a:1")
		return us[len(us)-1].Source == SourceA2S && us[len(us)-1].Err == nil
	})
	calls := fb.count()
	time.Sleep(40 * time.Millisecond)
	if fb.count() != calls {
		t.Fatal("the fallback was still asked although A2S works")
	}
}

func TestNoFallbackBehavesAsBefore(t *testing.T) {
	var col collector
	p := &Poller{Dial: a2sDown, Interval: 3 * time.Millisecond, OnUpdate: col.add}
	defer run(t, p)()
	waitFor(t, func() bool {
		for _, u := range col.get("a:1") {
			if u.Transition && !u.Online {
				return true
			}
		}
		return false
	})
}
