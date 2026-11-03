package rules

import (
	"testing"
	"time"
)

func TestParseSchedule(t *testing.T) {
	s, err := ParseSchedule("20:00-23:30", "0000011")
	if err != nil || s.StartMin != 20*60 || s.EndMin != 23*60+30 || s.Mask() != "0000011" {
		t.Fatalf("%+v %v", s, err)
	}
	if got := s.String(); got != "Sat, Sun 20:00-23:30 UTC" {
		t.Errorf("String() = %q", got)
	}
	if s, err = ParseSchedule(" 8:05 - 9:00 ", ""); err != nil || s.StartMin != 8*60+5 || s.DaysText() != "every day" {
		t.Errorf("spaces and default days: %+v %v", s, err)
	}
	for _, tc := range []struct{ window, days string }{
		{"20:00", ""}, {"25:00-26:00", ""}, {"10:60-11:00", ""}, {"ab:cd-10:00", ""}, {"10:00-10:00", ""},
		{"10:00-11:00", "111"}, {"10:00-11:00", "0000000"}, {"10:00-11:00", "11x1111"}, {"1000-1100", ""},
	} {
		if _, err := ParseSchedule(tc.window, tc.days); err == nil {
			t.Errorf("%q %q should be refused", tc.window, tc.days)
		}
	}
}

func TestScheduleOpen(t *testing.T) {
	// t0 is a Tuesday. Days run Monday first.
	at := func(day, h, m int) time.Time { return time.Date(2026, 10, 26+day, h, m, 0, 0, time.UTC) } // day 0 = Monday
	weekend, _ := ParseSchedule("20:00-23:00", "0000011")
	for _, tc := range []struct {
		when time.Time
		want bool
	}{
		{at(5, 20, 0), true}, {at(5, 22, 59), true}, {at(5, 23, 0), false}, {at(5, 19, 59), false},
		{at(6, 21, 0), true}, {at(1, 21, 0), false}, {at(4, 21, 0), false},
	} {
		if got := weekend.Open(tc.when); got != tc.want {
			t.Errorf("weekend window at %v = %v, want %v", tc.when, got, tc.want)
		}
	}
	// A window past midnight belongs to the day it opens on.
	night, _ := ParseSchedule("22:00-02:00", "0000010") // opens Saturday
	for _, tc := range []struct {
		when time.Time
		want bool
	}{
		{at(5, 23, 0), true}, {at(6, 1, 59), true}, {at(6, 2, 0), false}, {at(6, 23, 0), false}, {at(5, 1, 0), false},
	} {
		if got := night.Open(tc.when); got != tc.want {
			t.Errorf("night window at %v = %v, want %v", tc.when, got, tc.want)
		}
	}
	// Other timezones are converted to UTC first.
	est := time.FixedZone("x", -5*3600)
	if !weekend.Open(time.Date(2026, 10, 31, 16, 0, 0, 0, est)) { // 21:00 UTC Saturday
		t.Error("the window is in UTC whatever zone the time is in")
	}
}

func TestOutsideTheWindowNothingFiresAndItStartsOver(t *testing.T) {
	win, _ := ParseSchedule("12:10-13:00", "")
	n := quick(notif(mapIs("a")))
	n.Schedule = &win
	// t0 is 12:00, then a minute each: the window opens at step 10.
	n.Phase = PhaseArmed
	var fired []int
	for i := 0; i < 14; i++ {
		mp := "b"
		if i >= 3 { // the map is "a" from step 3, before the window opens
			mp = "a"
		}
		var ev *Event
		n, ev = Evaluate(snap("T", mp, 0), n, t0.Add(time.Duration(i)*time.Minute))
		if ev != nil {
			fired = append(fired, i)
		}
		if i == 5 && n.Phase != PhaseNew {
			t.Errorf("outside the window it must start over, phase %v", n.Phase)
		}
	}
	// It was already true when the window opened, so it does not fire for that (E13d, E24).
	if len(fired) != 0 {
		t.Fatalf("fired at %v", fired)
	}
	// After it stops being true inside the window and becomes true again, it fires.
	n, _ = Evaluate(snap("T", "b", 0), n, t0.Add(20*time.Minute))
	if _, ev := Evaluate(snap("T", "a", 0), n, t0.Add(21*time.Minute)); ev == nil {
		t.Error("inside the window it fires when the conditions become true")
	}
}
