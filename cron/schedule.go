package cron

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// scheduleParser is the ONE grammar every caller accepts: the same parser the
// Scheduler installs, so a preview cannot accept an expression the scheduler
// would reject (or vice versa).
var scheduleParser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)

// Location resolves a job's timezone. Empty means the machine's local zone.
func Location(tz string) (*time.Location, error) {
	tz = strings.TrimSpace(tz)
	if tz == "" {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("时区 %q 不认识：%w", tz, err)
	}
	return loc, nil
}

// LocalTimezoneName is the IANA name behind time.Local when the OS makes it
// discoverable (TZ, or the /etc/localtime symlink), else "Local". The desktop
// uses it to say 「本机（Asia/Shanghai）」 instead of 「本机（Local）」 — the
// latter is Go's spelling of the same thing, not a zone a reader knows.
func LocalTimezoneName() string {
	if tz := strings.TrimSpace(os.Getenv("TZ")); tz != "" {
		return tz
	}
	if link, err := os.Readlink("/etc/localtime"); err == nil {
		const marker = "zoneinfo/"
		if i := strings.LastIndex(link, marker); i >= 0 {
			return link[i+len(marker):]
		}
	}
	return time.Local.String()
}

// NextRun returns the first occurrence of schedule strictly after now, in the
// job's timezone. It is the ONE implementation behind the desk panel's preview,
// the tool's output and the scheduler's own next-run display.
func NextRun(schedule, tz string, now time.Time) (time.Time, error) {
	sched, err := scheduleParser.Parse(schedule)
	if err != nil {
		return time.Time{}, fmt.Errorf("时间表达式 %q 不合法：%w", schedule, err)
	}
	loc, err := Location(tz)
	if err != nil {
		return time.Time{}, err
	}
	return sched.Next(now.In(loc)), nil
}

// NextRuns returns up to n occurrences after now. A schedule that produces no
// time within the library's own horizon yields fewer (possibly zero).
func NextRuns(schedule, tz string, now time.Time, n int) ([]time.Time, error) {
	sched, err := scheduleParser.Parse(schedule)
	if err != nil {
		return nil, fmt.Errorf("时间表达式 %q 不合法：%w", schedule, err)
	}
	loc, err := Location(tz)
	if err != nil {
		return nil, err
	}
	out := make([]time.Time, 0, n)
	t := now.In(loc)
	for i := 0; i < n; i++ {
		t = sched.Next(t)
		if t.IsZero() {
			break
		}
		out = append(out, t)
	}
	return out, nil
}

// DescribeSchedule renders the SCHEDULE part of a cron expression in the
// words the desktop UI uses ("每天 09:00", "每周一至周五 09:00", "@every 2h").
// It deliberately covers only the shapes the UI can build; anything else is
// returned verbatim, which is better than a wrong sentence.
//
// It says nothing about the timezone: that is a separate field and the caller
// prints it beside this text.
func DescribeSchedule(expr string) string {
	s := strings.TrimSpace(expr)
	switch s {
	case "@yearly", "@annually":
		return "每年 1 月 1 日 00:00"
	case "@monthly":
		return "每月 1 日 00:00"
	case "@weekly":
		return "每周日 00:00"
	case "@daily", "@midnight":
		return "每天 00:00"
	case "@hourly":
		return "每小时"
	}
	if rest, ok := strings.CutPrefix(s, "@every "); ok {
		if d, err := time.ParseDuration(strings.TrimSpace(rest)); err == nil {
			return "每 " + humanDuration(d)
		}
		return s
	}

	fields := strings.Fields(s)
	if len(fields) != 5 {
		return s
	}
	minute, hour, dom, month, dow := fields[0], fields[1], fields[2], fields[3], fields[4]
	if minute == "*" && hour == "*" && dom == "*" && month == "*" && dow == "*" {
		return "每分钟"
	}
	if n, ok := everyN(minute); ok && hour == "*" && dom == "*" && month == "*" && dow == "*" {
		return fmt.Sprintf("每 %d 分钟", n)
	}
	if dom != "*" || month != "*" {
		return s
	}
	// The editor's "每 N 小时" preset builds `0 */N * * *`.
	if n, ok := everyN(hour); ok && dow == "*" {
		return fmt.Sprintf("每 %d 小时", n)
	}
	if hour == "*" && dow == "*" && minute == "0" {
		return "每小时"
	}
	hm, ok := clock(minute, hour)
	if !ok {
		return s
	}
	switch {
	case hour == "*" && dow == "*":
		// A fixed minute at an unspecified hour is still a per-hour schedule.
		if m, err := strconv.Atoi(minute); err == nil && m >= 0 {
			return fmt.Sprintf("每小时第 %d 分钟", m)
		}
		return s
	case dow == "*":
		return "每天 " + hm
	case hour == "*":
		return s
	}
	if days, ok := describeWeekdays(dow); ok {
		return "每" + days + " " + hm
	}
	return s
}

// everyN recognises "*/N" and returns N.
func everyN(field string) (int, bool) {
	rest, ok := strings.CutPrefix(field, "*/")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// clock renders a minute/hour pair as HH:MM. Anything that is not a plain
// number in range is not a clock, and the caller falls back to the raw
// expression.
func clock(minute, hour string) (string, bool) {
	m, err := strconv.Atoi(minute)
	if err != nil || m < 0 || m > 59 {
		return "", false
	}
	h, err := strconv.Atoi(hour)
	if err != nil || h < 0 || h > 23 {
		return "", false
	}
	return fmt.Sprintf("%02d:%02d", h, m), true
}

// describeWeekdays renders the day-of-week field in Chinese. It understands
// the shapes the UI builds (a single day, a comma list, and a single range);
// anything else declines, and the raw expression is shown instead.
func describeWeekdays(field string) (string, bool) {
	names := func(day int) (string, bool) {
		switch day {
		case 0, 7:
			return "周日", true
		case 1:
			return "周一", true
		case 2:
			return "周二", true
		case 3:
			return "周三", true
		case 4:
			return "周四", true
		case 5:
			return "周五", true
		case 6:
			return "周六", true
		}
		return "", false
	}

	if a, b, ok := strings.Cut(field, "-"); ok {
		from, err1 := strconv.Atoi(a)
		to, err2 := strconv.Atoi(b)
		if err1 != nil || err2 != nil {
			return "", false
		}
		first, ok1 := names(from)
		last, ok2 := names(to)
		if !ok1 || !ok2 {
			return "", false
		}
		return first + "至" + last, true
	}

	parts := strings.Split(field, ",")
	if len(parts) == 1 {
		n, err := strconv.Atoi(parts[0])
		if err != nil {
			return "", false
		}
		return names(n)
	}
	rendered := make([]string, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return "", false
		}
		name, ok := names(n)
		if !ok {
			return "", false
		}
		rendered = append(rendered, name)
	}
	return strings.Join(rendered, "、"), true
}

// humanDuration is the short form used in "每 …": hours when it divides
// evenly, otherwise minutes, otherwise the library's own string.
func humanDuration(d time.Duration) string {
	if d <= 0 {
		return d.String()
	}
	if d%time.Hour == 0 {
		return fmt.Sprintf("%d 小时", int(d/time.Hour))
	}
	if d%time.Minute == 0 {
		return fmt.Sprintf("%d 分钟", int(d/time.Minute))
	}
	return d.String()
}
