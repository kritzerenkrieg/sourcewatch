package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/kritzerenkrieg/sourcewatch/internal/rules"
)

// fakeDiscord records requests and answers from a script keyed by "METHOD path-suffix".
type fakeDiscord struct {
	mu      sync.Mutex
	reqs    []rec
	handler func(r rec, n int) (status int, body string)
}

type rec struct {
	Method, Path string
	Body         []byte
}

func (f *fakeDiscord) count(suffix string) (n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.reqs {
		if strings.HasSuffix(r.Path, suffix) {
			n++
		}
	}
	return
}

func (f *fakeDiscord) last(suffix string) rec {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.reqs) - 1; i >= 0; i-- {
		if strings.HasSuffix(f.reqs[i].Path, suffix) {
			return f.reqs[i]
		}
	}
	return rec{}
}

func newNotifier(t *testing.T, h func(r rec, n int) (int, string)) (*Discord, *fakeDiscord) {
	t.Helper()
	f := &fakeDiscord{handler: h}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, rec{r.Method, r.URL.Path, b})
		n := len(f.reqs)
		cur := f.reqs[n-1]
		f.mu.Unlock()
		status, body := f.handler(cur, n)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	s, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatal(err)
	}
	s.Client = &http.Client{Transport: rewrite{u}}
	return &Discord{s: s}, f
}

// rewrite sends every request to the test server whatever host discordgo asked for.
type rewrite struct{ to *url.URL }

func (rw rewrite) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host = rw.to.Scheme, rw.to.Host
	return http.DefaultTransport.RoundTrip(r)
}

func ok(r rec, n int) (int, string) {
	if strings.HasSuffix(r.Path, "/users/@me/channels") {
		return 200, `{"id":"900"}`
	}
	return 200, `{"id":"1","channel_id":"900"}`
}

type sent struct {
	Content         string `json:"content"`
	Flags           int    `json:"flags"`
	AllowedMentions *struct {
		Parse []string `json:"parse"`
		Users []string `json:"users"`
	} `json:"allowed_mentions"`
}

func body(t *testing.T, r rec) (sent, string) {
	t.Helper()
	var m sent
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatalf("bad body %q: %v", r.Body, err)
	}
	return m, string(r.Body)
}

func TestChannelMentionsOnlyThePlayerByID(t *testing.T) {
	d, f := newNotifier(t, ok)
	err := d.Send(context.Background(), Target{Delivery: rules.DeliveryChannel, ChannelID: "555", UserID: "123"},
		"Map change", "@everyone <@999> joined @here")
	if err != nil {
		t.Fatal(err)
	}
	r := f.last("/channels/555/messages")
	m, raw := body(t, r)
	if !strings.HasPrefix(m.Content, "<@123> ") {
		t.Errorf("content %q should start with the user mention", m.Content)
	}
	if m.AllowedMentions == nil || m.AllowedMentions.Parse == nil || len(m.AllowedMentions.Parse) != 0 {
		t.Errorf("allowed_mentions.parse must be an empty list: %s", raw)
	}
	if len(m.AllowedMentions.Users) != 1 || m.AllowedMentions.Users[0] != "123" {
		t.Errorf("allowed_mentions.users must be only the player: %s", raw)
	}
	if !strings.Contains(m.Content, "@everyone") {
		t.Errorf("name should stay as plain text: %q", m.Content)
	}
	if m.Flags&4 == 0 {
		t.Errorf("link previews must be suppressed (flag 4): %s", raw)
	}
}

func TestEmptyDeliveryMeansChannel(t *testing.T) {
	d, f := newNotifier(t, ok)
	if err := d.Send(context.Background(), Target{ChannelID: "555", UserID: "123"}, "t", "m"); err != nil {
		t.Fatal(err)
	}
	if f.count("/channels/555/messages") != 1 {
		t.Fatal("default delivery should post in the channel")
	}
}

func TestDMCreatesChannelThenSendsWithNoMentions(t *testing.T) {
	d, f := newNotifier(t, ok)
	if err := d.Send(context.Background(), Target{Delivery: rules.DeliveryDM, UserID: "123"}, "t", "@everyone"); err != nil {
		t.Fatal(err)
	}
	if f.count("/users/@me/channels") != 1 {
		t.Fatal("DM channel not created")
	}
	if !strings.Contains(string(f.last("/users/@me/channels").Body), `"recipient_id":"123"`) {
		t.Errorf("wrong recipient: %s", f.last("/users/@me/channels").Body)
	}
	m, raw := body(t, f.last("/channels/900/messages"))
	if m.AllowedMentions == nil || len(m.AllowedMentions.Parse) != 0 || len(m.AllowedMentions.Users) != 0 {
		t.Errorf("DM must allow no mentions: %s", raw)
	}
	if strings.Contains(m.Content, "<@123>") {
		t.Errorf("DM should not mention the player: %q", m.Content)
	}
}

func TestRejectsUnsafeIDs(t *testing.T) {
	d, f := newNotifier(t, ok)
	for _, tg := range []Target{
		{ChannelID: "555", UserID: "123> @everyone <@1"},
		{ChannelID: "555", UserID: ""},
		{ChannelID: "5/5", UserID: "123"},
		{Delivery: "pigeon", ChannelID: "555", UserID: "123"},
	} {
		if err := d.Send(context.Background(), tg, "t", "m"); err == nil {
			t.Errorf("%+v should be rejected", tg)
		}
	}
	if len(f.reqs) != 0 {
		t.Fatal("nothing should be sent")
	}
}

func TestLongMessageIsTruncated(t *testing.T) {
	d, f := newNotifier(t, ok)
	d.Send(context.Background(), Target{ChannelID: "555", UserID: "123"}, "t", strings.Repeat("x", 5000))
	m, _ := body(t, f.last("/channels/555/messages"))
	if n := len([]rune(m.Content)); n > maxContent {
		t.Fatalf("content has %d runes", n)
	}
}

const closed = `{"code":50007,"message":"Cannot send messages to this user"}`

func TestClosedDMOnSend(t *testing.T) {
	d, _ := newNotifier(t, func(r rec, n int) (int, string) {
		if strings.HasSuffix(r.Path, "/users/@me/channels") {
			return 200, `{"id":"900"}`
		}
		return 403, closed
	})
	err := d.Send(context.Background(), Target{Delivery: rules.DeliveryDM, UserID: "123"}, "t", "m")
	if !errors.Is(err, ErrDMClosed) {
		t.Fatalf("got %v, want ErrDMClosed", err)
	}
}

func TestClosedDMOnChannelCreate(t *testing.T) {
	d, _ := newNotifier(t, func(rec, int) (int, string) { return 403, closed })
	if err := d.ProbeDM(context.Background(), "123"); !errors.Is(err, ErrDMClosed) {
		t.Fatalf("got %v, want ErrDMClosed", err)
	}
}

func TestRefusedChannelMessages(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{404, `{"code":10003,"message":"Unknown Channel"}`},
		{403, `{"code":50001,"message":"Missing Access"}`},
		{403, `{"code":50013,"message":"Missing Permissions"}`},
	} {
		d, f := newNotifier(t, func(rec, int) (int, string) { return tc.status, tc.body })
		err := d.Send(context.Background(), Target{ChannelID: "555", UserID: "123"}, "t", "m")
		if !errors.Is(err, ErrChannelRefused) {
			t.Errorf("%s: got %v, want ErrChannelRefused", tc.body, err)
		}
		if len(f.reqs) != 1 {
			t.Errorf("%s: %d requests, want 1", tc.body, len(f.reqs))
		}
	}
}

func TestProbeDMSendsOneDM(t *testing.T) {
	d, f := newNotifier(t, ok)
	if err := d.ProbeDM(context.Background(), "123"); err != nil {
		t.Fatal(err)
	}
	if f.count("/channels/900/messages") != 1 {
		t.Fatal("probe should send one DM")
	}
}

func TestRateLimitIsWaitedOut(t *testing.T) {
	d, f := newNotifier(t, func(r rec, n int) (int, string) {
		if n == 1 {
			return 429, `{"message":"You are being rate limited.","retry_after":0.05,"global":false}`
		}
		return 200, `{"id":"1"}`
	})
	if err := d.Send(context.Background(), Target{ChannelID: "555", UserID: "123"}, "t", "m"); err != nil {
		t.Fatalf("429 should be retried: %v", err)
	}
	if got := f.count("/channels/555/messages"); got != 2 {
		t.Fatalf("%d requests, want 2 (429 then success)", got)
	}
}

func TestUnauthorizedStops(t *testing.T) {
	d, f := newNotifier(t, func(rec, int) (int, string) { return 401, `{"code":0,"message":"401: Unauthorized"}` })
	err := d.Send(context.Background(), Target{ChannelID: "555", UserID: "123"}, "t", "m")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("got %v, want ErrUnauthorized", err)
	}
	if len(f.reqs) != 1 {
		t.Fatalf("%d requests, want 1: a 401 must not be retried", len(f.reqs))
	}
}

// --- Deliver ---

type scripted struct {
	errs  map[int64]error
	calls []int64
}

func (s *scripted) Send(_ context.Context, t Target, title, msg string) error {
	var id int64
	json.Unmarshal([]byte(msg), &id) // the message carries the notification id in these tests
	s.calls = append(s.calls, id)
	return s.errs[id]
}

type marks []int64

func (m *marks) MarkUndeliverable(id int64) error { *m = append(*m, id); return nil }

func ev(id int64) rules.Event {
	b, _ := json.Marshal(id)
	return rules.Event{NotificationID: id, UserID: "1", Message: string(b), Delivery: rules.DeliveryDM}
}

func TestDeliverMarksClosedDMsAndContinues(t *testing.T) {
	s := &scripted{errs: map[int64]error{2: ErrDMClosed}}
	var m marks
	if err := Deliver(context.Background(), s, &m, []rules.Event{ev(1), ev(2), ev(3)}); err != nil {
		t.Fatal(err)
	}
	if len(m) != 1 || m[0] != 2 {
		t.Fatalf("marked %v, want [2]", []int64(m))
	}
	if len(s.calls) != 3 {
		t.Fatalf("calls %v, want all three tried", s.calls)
	}
}

func TestDeliverMarksEveryoneInARefusedChannelMessage(t *testing.T) {
	// 7 and 8 are the same message in the same channel, so they are one message mentioning both (E16).
	s := &scripted{errs: map[int64]error{7: ErrChannelRefused}}
	var m marks
	events := []rules.Event{chEv(7, "1", "c", "t", "7"), chEv(8, "2", "c", "t", "7"), ev(3)}
	if err := Deliver(context.Background(), s, &m, events); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual([]int64(m), []int64{7, 8}) {
		t.Fatalf("marked %v, want [7 8]", []int64(m))
	}
	if len(s.calls) != 2 {
		t.Fatalf("calls %v, want the channel message and the DM", s.calls)
	}
}

func TestDeliverStopsOnUnauthorized(t *testing.T) {
	s := &scripted{errs: map[int64]error{1: ErrUnauthorized}}
	var m marks
	err := Deliver(context.Background(), s, &m, []rules.Event{ev(1), ev(2)})
	if !errors.Is(err, ErrUnauthorized) || len(s.calls) != 1 {
		t.Fatalf("err %v, calls %v", err, s.calls)
	}
}

func TestDeliverCollectsOtherErrors(t *testing.T) {
	s := &scripted{errs: map[int64]error{1: errors.New("boom")}}
	var m marks
	err := Deliver(context.Background(), s, &m, []rules.Event{ev(1), ev(2)})
	if err == nil || !strings.Contains(err.Error(), "boom") || len(s.calls) != 2 || len(m) != 0 {
		t.Fatalf("err %v, calls %v, marks %v", err, s.calls, m)
	}
}

var _ Notifier = (*Discord)(nil)
var _ Notifier = (*scripted)(nil)

// --- people with the same notification are mentioned together ---

func chEv(id int64, user, channel, title, msg string) rules.Event {
	return rules.Event{NotificationID: id, UserID: user, Title: title, Message: msg, Delivery: rules.DeliveryChannel, ChannelID: channel}
}

func TestSameMessageInTheSameChannelMentionsEveryoneOnce(t *testing.T) {
	d, f := newNotifier(t, ok)
	events := []rules.Event{
		chEv(1, "111", "555", "Map change", "`T` changed map to `m`."),
		chEv(2, "222", "555", "Map change", "`T` changed map to `m`."),
		chEv(3, "333", "555", "Map change", "`T` changed map to `m`."),
		chEv(4, "111", "555", "Map change", "`T` changed map to `m`."), // the same person twice
	}
	if err := Deliver(context.Background(), d, &marks{}, events); err != nil {
		t.Fatal(err)
	}
	if got := f.count("/channels/555/messages"); got != 1 {
		t.Fatalf("%d messages, want one for all of them", got)
	}
	m, raw := body(t, f.last("/channels/555/messages"))
	if !strings.HasPrefix(m.Content, "<@111> <@222> <@333> **Map change**") {
		t.Errorf("content %q", m.Content)
	}
	if strings.Count(m.Content, "<@111>") != 1 {
		t.Errorf("a person is mentioned once: %q", m.Content)
	}
	if m.AllowedMentions == nil || len(m.AllowedMentions.Parse) != 0 || !reflect.DeepEqual(m.AllowedMentions.Users, []string{"111", "222", "333"}) {
		t.Errorf("only those people may be pinged, and nothing else: %s", raw)
	}
}

func TestDifferentMessagesOrChannelsAreSeparate(t *testing.T) {
	d, f := newNotifier(t, ok)
	events := []rules.Event{
		chEv(1, "111", "555", "Map change", "map one"),
		chEv(2, "222", "555", "Map change", "map two"),    // other text
		chEv(3, "333", "666", "Map change", "map one"),    // other channel
		chEv(4, "444", "555", "Server status", "map one"), // other title
		chEv(5, "555", "555", "Map change", "map one"),    // joins the first
	}
	if err := Deliver(context.Background(), d, &marks{}, events); err != nil {
		t.Fatal(err)
	}
	if got := f.count("/channels/555/messages"); got != 3 {
		t.Errorf("%d messages in channel 555, want 3", got)
	}
	if got := f.count("/channels/666/messages"); got != 1 {
		t.Errorf("%d messages in channel 666, want 1", got)
	}
}

func TestDMsAreNeverMerged(t *testing.T) {
	groups := groupEvents([]rules.Event{
		{NotificationID: 1, UserID: "111", Title: "T", Message: "m", Delivery: rules.DeliveryDM},
		{NotificationID: 2, UserID: "222", Title: "T", Message: "m", Delivery: rules.DeliveryDM},
	})
	if len(groups) != 2 || len(groups[0].target.UserIDs) != 0 {
		t.Fatalf("%+v", groups)
	}
}

func TestAHugeGroupIsSplitIntoSeveralMessages(t *testing.T) {
	var events []rules.Event
	for i := 0; i < 2*MaxMentions+5; i++ {
		events = append(events, chEv(int64(i), fmt.Sprint(1000+i), "555", "Server status", "`T` went offline."))
	}
	groups := groupEvents(events)
	if len(groups) != 3 || len(groups[0].target.UserIDs) != MaxMentions || len(groups[2].target.UserIDs) != 5 {
		t.Fatalf("group sizes: %d groups", len(groups))
	}
	d, f := newNotifier(t, ok)
	if err := Deliver(context.Background(), d, &marks{}, events); err != nil {
		t.Fatal(err)
	}
	if f.count("/channels/555/messages") != 3 {
		t.Fatalf("%d messages", f.count("/channels/555/messages"))
	}
	for _, r := range f.reqs {
		if m, _ := body(t, r); len([]rune(m.Content)) > maxContent {
			t.Errorf("a message has %d characters", len([]rune(m.Content)))
		}
	}
}

func TestGroupedSendRejectsAnUnsafeUserID(t *testing.T) {
	d, f := newNotifier(t, ok)
	err := d.Send(context.Background(), Target{ChannelID: "555", UserID: "111", UserIDs: []string{"111", "222> @everyone <@1"}}, "t", "m")
	if err == nil || len(f.reqs) != 0 {
		t.Fatalf("an unsafe ID in a group must stop the message: %v, %d requests", err, len(f.reqs))
	}
}

func TestAFailedGroupReportsAllItsNotifications(t *testing.T) {
	boom := errors.New("boom")
	n := &failingNotifier{err: boom}
	err := Deliver(context.Background(), n, &marks{}, []rules.Event{
		chEv(7, "111", "555", "T", "m"), chEv(8, "222", "555", "T", "m"),
	})
	if err == nil || !strings.Contains(err.Error(), "[7 8]") || n.calls != 1 {
		t.Fatalf("err %v, calls %d: one message, and both notifications named in the error", err, n.calls)
	}
}

type failingNotifier struct {
	err   error
	calls int
}

func (f *failingNotifier) Send(context.Context, Target, string, string) error {
	f.calls++
	return f.err
}
