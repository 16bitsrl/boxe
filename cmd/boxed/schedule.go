package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

// A sleeping VM still runs the jobs scheduled in it: before it goes to
// sleep boxed asks boxe-agent what is scheduled (cron, systemd timers),
// works out when the next job runs and wakes the VM up shortly before it.
// The guest clock is right as soon as the VM wakes up: kvm-clock follows
// the host's. Once the job is done, the VM goes back to sleep. A job missed
// while asleep (a wake that did not fit) runs late at the next wake: systemd
// runs timers that elapsed, cron the jobs of a clock that jumped ahead.

const (
	wakeLead = time.Minute      // a VM wakes up this long before its job
	jobIdle  = 2 * time.Minute  // and sleeps again after this long idle, from the job's time: systemd runs timers up to a minute late (AccuracySec)
	minNap   = 10 * time.Minute // an idle VM with a job due sooner stays awake for it
)

// schedule is what boxe-agent in v says is scheduled, raw; empty if it
// cannot tell (an older image).
func (b *Box) schedule(v VM) string {
	answer, err := b.hv.ask(v.Name, "schedule", 5*time.Second)
	if err != nil && !strings.Contains(err.Error(), "unknown command") && !errors.Is(err, errAgentUnreachable) {
		log.Printf("%s: schedule of %s: %v", v.Owner, v.Name, err)
	}
	if err != nil {
		return ""
	}
	return answer
}

// nextJob is when the first job the user scheduled in the VM runs after
// now, and what it is, from boxe-agent's answer to schedule; zero if none.
//
// Only what the user added counts: their crontabs, files in /etc/cron.d
// that no package installed (boxe-agent leaves those out) and timers with
// their unit file where admins and users put them (see userTimer). Ubuntu's
// own (logrotate, fstrim, apt, man-db, /etc/crontab's cron.daily...) would
// wake every VM every day. @reboot runs only at boot.
func nextJob(answer string, now time.Time) (time.Time, string) {
	loc := time.UTC
	var next time.Time
	var what string
	found := func(t time.Time, w string) {
		if t.After(now) && (next.IsZero() || t.Before(next)) {
			next, what = t, w
		}
	}
	type timer struct {
		Next *int64 `json:"next"` // µs since the epoch
		Unit string `json:"unit"`
	}
	timers := map[string][]timer{}             // scope -> timers
	units := map[string]map[string][2]string{} // scope -> unit -> fragment path, load state
	var kind, name string
	var body []string
	flush := func() {
		text := strings.Join(body, "\n")
		switch kind {
		case "list-timers":
			var list []timer
			if json.Unmarshal([]byte(text), &list) == nil {
				timers[name] = list
			}
		case "units":
			m := map[string][2]string{}
			for _, block := range strings.Split(text, "\n\n") {
				var id, path, load string
				for _, l := range strings.Split(block, "\n") {
					k, v, _ := strings.Cut(l, "=")
					switch k {
					case "Id":
						id = v
					case "FragmentPath":
						path = v
					case "LoadState":
						load = v
					}
				}
				m[id] = [2]string{path, load}
			}
			units[name] = m
		case "crontab", "cron.d":
			src := "cron of " + name
			if kind == "cron.d" {
				src = "cron.d/" + name
			}
			for _, line := range body {
				line = strings.TrimSpace(line)
				spec := cronSpec(line)
				if spec == "" {
					continue
				}
				if s, err := parseCron(spec); err == nil {
					found(s.next(now.In(loc)), src+": "+line)
				}
			}
		}
		body = nil
	}
	for _, line := range strings.Split(answer, "\n") {
		if l, ok := strings.CutPrefix(line, "localtime "); ok && kind == "" {
			if _, zone, ok := strings.Cut(l, "zoneinfo/"); ok {
				if z, err := time.LoadLocation(zone); err == nil {
					loc = z
				}
			}
			continue
		}
		if l, ok := strings.CutPrefix(line, "--- "); ok {
			flush()
			kind, name, _ = strings.Cut(l, " ")
			continue
		}
		body = append(body, line)
	}
	flush()
	for scope, list := range timers {
		for _, t := range list {
			u := units[scope][t.Unit]
			if t.Next == nil || !userTimer(u[0], u[1]) {
				continue
			}
			w := "timer " + t.Unit
			if user, ok := strings.CutPrefix(scope, "user "); ok {
				w += " of " + user
			}
			found(time.UnixMicro(*t.Next), w)
		}
	}
	return next, what
}

// userTimer: a timer whose unit file is where admins and users put theirs,
// or one made with systemd-run; not one a package installed (/usr/lib).
// exeuntu masks most of Ubuntu's timers in /etc/systemd/system: masked ones
// do not count.
func userTimer(path, load string) bool {
	if load != "loaded" {
		return false
	}
	for _, p := range []string{"/etc/systemd/", "/usr/local/lib/systemd/", "/run/systemd/transient/"} {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	if strings.HasPrefix(path, "/run/user/") && strings.Contains(path, "/systemd/transient/") {
		return true
	}
	if strings.HasPrefix(path, "/home/") || strings.HasPrefix(path, "/root/") {
		return strings.Contains(path, "/.config/systemd/user/") || strings.Contains(path, "/.local/share/systemd/user/")
	}
	return false
}

// cronSpec is the schedule part of a crontab line (five fields or an @
// word); empty for comments, variables and blank lines.
func cronSpec(line string) string {
	f := strings.Fields(line)
	if len(f) == 0 || strings.HasPrefix(f[0], "#") || strings.Contains(f[0], "=") {
		return ""
	}
	if strings.HasPrefix(f[0], "@") {
		return f[0]
	}
	if len(f) < 6 {
		return ""
	}
	return strings.Join(f[:5], " ")
}

// cronSchedule is a cron schedule: bit n of each field is set if the value
// n matches. As in cron, if both days are restricted (neither starts with
// *), either one matching is enough.
type cronSchedule struct {
	min, hour, dom, month, dow uint64
	eitherDay                  bool
}

var cronWords = map[string]string{
	"@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *", "@monthly": "0 0 1 * *",
	"@weekly": "0 0 * * 0", "@daily": "0 0 * * *", "@midnight": "0 0 * * *", "@hourly": "0 * * * *",
}

var (
	monthNames = []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}
	dayNames   = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}
)

func parseCron(spec string) (cronSchedule, error) {
	if s, ok := cronWords[spec]; ok {
		spec = s
	}
	f := strings.Fields(spec)
	if len(f) != 5 {
		return cronSchedule{}, fmt.Errorf("not a cron schedule: %q", spec)
	}
	var s cronSchedule
	var err error
	for i, p := range []struct {
		to       *uint64
		min, max int
		names    []string
		base     int // value of the first name
	}{{&s.min, 0, 59, nil, 0}, {&s.hour, 0, 23, nil, 0}, {&s.dom, 1, 31, nil, 0},
		{&s.month, 1, 12, monthNames, 1}, {&s.dow, 0, 7, dayNames, 0}} {
		if *p.to, err = cronField(f[i], p.min, p.max, p.names, p.base); err != nil {
			return cronSchedule{}, fmt.Errorf("%q: %v", spec, err)
		}
	}
	if s.dow&(1<<7) != 0 { // 7 is Sunday too
		s.dow |= 1
	}
	s.eitherDay = !strings.HasPrefix(f[2], "*") && !strings.HasPrefix(f[4], "*")
	return s, nil
}

// cronField parses a list of values, ranges (a-b) and steps (*/n, a-b/n).
func cronField(field string, lo, hi int, names []string, base int) (uint64, error) {
	num := func(s string) (int, error) {
		for i, n := range names {
			if strings.EqualFold(s, n) {
				return base + i, nil
			}
		}
		n, err := strconv.Atoi(s)
		if err != nil || n < lo || n > hi {
			return 0, fmt.Errorf("%q out of %d-%d", s, lo, hi)
		}
		return n, nil
	}
	var bits uint64
	for _, item := range strings.Split(field, ",") {
		rng, stepS, hasStep := strings.Cut(item, "/")
		step := 1
		if hasStep {
			var err error
			if step, err = strconv.Atoi(stepS); err != nil || step <= 0 {
				return 0, fmt.Errorf("bad step %q", item)
			}
		}
		from, to := lo, hi
		if rng != "*" {
			a, b, isRange := strings.Cut(rng, "-")
			var err error
			if from, err = num(a); err != nil {
				return 0, err
			}
			to = from
			if isRange {
				if to, err = num(b); err != nil {
					return 0, err
				}
			} else if hasStep {
				to = hi // a/n: from a to the end
			}
			if to < from {
				return 0, fmt.Errorf("bad range %q", item)
			}
		}
		for n := from; ; n += step {
			bits |= 1 << n
			if step > to-n { // not n+step > to: a huge step would overflow
				break
			}
		}
	}
	return bits, nil
}

// next is the first time after t (in t's time zone) that s matches.
func (s cronSchedule) next(t time.Time) time.Time {
	loc := t.Location()
	t = t.Truncate(time.Minute).Add(time.Minute)
	// to is the start of the next month, day or hour, in local time; where
	// that time does not exist (summer time starts) it may not be later than
	// t, so then an hour on: every step moves on and the search ends.
	to := func(next time.Time) time.Time {
		if next.After(t) {
			return next
		}
		return t.Add(time.Hour - time.Duration(t.Minute())*time.Minute)
	}
	for end := t.AddDate(5, 0, 0); t.Before(end); {
		if s.month&(1<<int(t.Month())) == 0 {
			t = to(time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, loc))
			continue
		}
		dom, dow := s.dom&(1<<t.Day()) != 0, s.dow&(1<<int(t.Weekday())) != 0
		if day := dom && dow; !day && !(s.eitherDay && (dom || dow)) {
			t = to(time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc))
			continue
		}
		if s.hour&(1<<t.Hour()) == 0 {
			t = to(time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, loc))
			continue
		}
		if s.min&(1<<t.Minute()) == 0 {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}
	return time.Time{}
}

// jobRetry is how soon a VM that could not wake up for a job tries again.
const jobRetry = 15 * time.Minute

// wakeJobs wakes the sleeping VMs whose next job is due within wakeLead.
func (b *Box) wakeJobs(now time.Time) {
	vms, err := b.store.List("")
	if err != nil {
		log.Printf("wake for jobs: %v", err)
		return
	}
	for _, v := range vms {
		if !v.NextJob.IsZero() && !v.NextJob.Add(-wakeLead).After(now) && b.hv.Sleeping(v.Name) {
			b.wakeForJob(v, now)
		}
	}
}

// wakeForJob wakes v for its next job. If it does not fit in its owner's
// RAM, or does not wake up, it skips that run and waits for the next one.
func (b *Box) wakeForJob(v VM, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cur, err := b.store.Get(v.Name)
	if err != nil || !cur.same(v) || !cur.NextJob.Equal(v.NextJob) || !b.hv.Sleeping(v.Name) {
		return
	}
	_, what := nextJob(cur.Schedule, cur.NextJob.Add(-time.Second))
	err = b.makeRoom(cur)
	if err == nil {
		err = b.hv.Wake(cur)
	}
	if err != nil {
		// Next time: its next run, or another try soon. A timer's next run
		// is known only to the VM, so a skipped one would never come again.
		next, _ := nextJob(cur.Schedule, cur.NextJob)
		if retry := now.Add(jobRetry); next.IsZero() || next.After(retry) {
			next = retry
		}
		log.Printf("%s: %s did not wake up for %s at %s, next at %s: %v", cur.Owner, cur.Name, what,
			cur.NextJob.Local().Format(time.TimeOnly), next.Local().Format(time.DateTime), err)
		if err := b.store.SetNextJob(cur.Name, next, cur.Schedule); err != nil {
			log.Printf("wake for jobs: %v", err)
		}
		return
	}
	log.Printf("%s: %s woke up for %s at %s", cur.Owner, cur.Name, what, cur.NextJob.Local().Format(time.TimeOnly))
	b.actMu.Lock()
	a := b.activityOf(cur.Name, now)
	a.last, a.job = now, cur.NextJob
	b.actMu.Unlock()
}

// wakeTime is when a VM wakes up, for people: the time within a day, else
// the date too.
func wakeTime(t, now time.Time) string {
	if t.Sub(now) < 24*time.Hour {
		return t.Local().Format("15:04")
	}
	return t.Local().Format("2006-01-02 15:04")
}

// WakeLoop wakes sleeping VMs for their jobs, checking every so often.
func (b *Box) WakeLoop(every time.Duration) {
	for {
		time.Sleep(every)
		b.wakeJobs(time.Now())
	}
}
