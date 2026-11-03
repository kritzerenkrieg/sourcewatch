package rules

import (
	"fmt"
	"strings"
	"time"
)

// Schedule limits when a notification can fire: a window of the day, on chosen weekdays, in UTC (E24).
// Outside the window the notification is skipped and starts over, like a disabled one that is enabled
// again, so it never fires for conditions that already held when the window opened.
type Schedule struct {
	StartMin int     // minutes after 00:00 UTC the window opens
	EndMin   int     // minutes after 00:00 UTC it closes (exclusive); smaller than StartMin means past midnight
	Days     [7]bool // Monday first. A window that crosses midnight belongs to the day it opens on
}

var dayNames = [7]string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}

// AllDays is the days option when none is given.
const AllDays = "1111111"

// ParseSchedule reads "HH:MM-HH:MM" (UTC) and a days mask of seven 0/1 characters, Monday first:
// "0000011" is Saturday and Sunday.
func ParseSchedule(window, days string) (Schedule, error) {
	var s Schedule
	window = strings.ReplaceAll(strings.TrimSpace(window), " ", "")
	from, to, ok := strings.Cut(window, "-")
	if !ok {
		return s, fmt.Errorf("the time is written HH:MM-HH:MM, like 18:00-23:30 (UTC)")
	}
	var err error
	if s.StartMin, err = parseClock(from); err != nil {
		return s, err
	}
	if s.EndMin, err = parseClock(to); err != nil {
		return s, err
	}
	if s.StartMin == s.EndMin {
		return s, fmt.Errorf("the window starts and ends at the same time; use `off` to remove a schedule")
	}
	if days = strings.TrimSpace(days); days == "" {
		days = AllDays
	}
	if len(days) != 7 {
		return s, fmt.Errorf("`day` is seven digits, Monday first, 1 for a day it may fire and 0 for a day it may not: `0000011` is Saturday and Sunday")
	}
	any := false
	for i, r := range days {
		switch r {
		case '0':
		case '1':
			s.Days[i], any = true, true
		default:
			return s, fmt.Errorf("`day` only holds 0 and 1: `0000011` is Saturday and Sunday")
		}
	}
	if !any {
		return s, fmt.Errorf("`day` needs at least one 1")
	}
	return s, nil
}

func parseClock(v string) (int, error) {
	var h, m int
	if len(v) < 4 || len(v) > 5 || !strings.Contains(v, ":") {
		return 0, fmt.Errorf("%q is not a time; write it HH:MM, like 18:00", v)
	}
	if _, err := fmt.Sscanf(v, "%d:%d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("%q is not a time; write it HH:MM, like 18:00", v)
	}
	return h*60 + m, nil
}

// weekday is Monday=0 ... Sunday=6.
func weekday(t time.Time) int { return (int(t.Weekday()) + 6) % 7 }

// Open reports whether t falls inside the window.
func (s Schedule) Open(t time.Time) bool {
	t = t.UTC()
	m, day := t.Hour()*60+t.Minute(), weekday(t)
	if s.StartMin < s.EndMin {
		return s.Days[day] && m >= s.StartMin && m < s.EndMin
	}
	if m >= s.StartMin && s.Days[day] { // before midnight, on the day it opened
		return true
	}
	return m < s.EndMin && s.Days[(day+6)%7] // after midnight, the day after
}

// Clock writes a minute of the day as HH:MM.
func Clock(min int) string { return fmt.Sprintf("%02d:%02d", min/60, min%60) }

// DaysText writes the days like "every day", "Sat, Sun" or "Mon, Tue".
func (s Schedule) DaysText() string {
	var out []string
	for i, on := range s.Days {
		if on {
			out = append(out, dayNames[i])
		}
	}
	if len(out) == 7 {
		return "every day"
	}
	return strings.Join(out, ", ")
}

// String is the schedule in plain text, for lists: "Sat, Sun 20:00-23:00 UTC".
func (s Schedule) String() string {
	return fmt.Sprintf("%s %s-%s UTC", s.DaysText(), Clock(s.StartMin), Clock(s.EndMin))
}

// Mask is the days as the seven digits ParseSchedule reads.
func (s Schedule) Mask() string {
	b := make([]byte, 7)
	for i, on := range s.Days {
		b[i] = '0'
		if on {
			b[i] = '1'
		}
	}
	return string(b)
}
