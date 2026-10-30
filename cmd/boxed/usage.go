package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// The charts on the page of a VM: CPU, RAM and disk over a range, from the
// samples of every minute (see Box.sample), averaged into usageBuckets
// points. The panel draws them as SVG; panel.js adds the readout on hover.

var usageRanges = map[string]time.Duration{"24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour}

// usageBuckets is how many points a chart has: one every 10 minutes over 24h.
const usageBuckets = 144

// chartW and chartH are the box the paths are drawn in, stretched to the plot.
const chartW, chartH = 1000, 100

type usage struct {
	Range  string   // 24h, 7d or 30d
	Ranges []string // to choose from
	From   string   // the start of the range, as the axis shows it
	Times  string   // the middle of each bucket, Unix seconds, for panel.js
	Charts []usageChart
}

type usageChart struct {
	Title      string
	Top        string // the value at the top of the chart
	Peak, Avg  string
	Line, Area string // SVG paths
	Dots       string // the points with no neighbor, which a line cannot show
	Values     string // one per bucket, "" where nothing was recorded, joined by |
}

// usageOf charts samples of v over rng (24h if unknown), up to now.
func usageOf(v vmJSON, samples []Sample, rng string, now time.Time) usage {
	span, ok := usageRanges[rng]
	if !ok {
		rng, span = "24h", usageRanges["24h"]
	}
	u := usage{Range: rng, Ranges: []string{"24h", "7d", "30d"}}
	start := now.Add(-span)
	if rng == "24h" {
		u.From = start.Local().Format("15:04")
	} else {
		u.From = start.Local().Format("Jan 2")
	}
	var sum [3][usageBuckets]float64
	var n [usageBuckets]int
	found := false
	for _, s := range samples {
		i := int(float64(s.At.Sub(start)) / float64(span) * usageBuckets)
		if i < 0 || i >= usageBuckets {
			continue
		}
		sum[0][i] += s.CPU
		sum[1][i] += float64(s.Mem)
		sum[2][i] += float64(s.Disk)
		n[i]++
		found = true
	}
	if !found {
		return u
	}
	times := make([]string, usageBuckets)
	for i := range times {
		mid := start.Add(time.Duration((float64(i) + 0.5) / usageBuckets * float64(span)))
		times[i] = strconv.FormatInt(mid.Unix(), 10)
	}
	u.Times = strings.Join(times, ",")

	ram := 0.0
	if mib, err := memMiB(v.Mem); err == nil {
		ram = float64(int64(mib) << 20)
	}
	cpu := func(x float64) string { return fmt.Sprintf("%.0f%%", x) }
	bytes := func(x float64) string { return size(int64(x)) }
	for k, c := range []struct {
		title  string
		format func(float64) string
		top    float64 // at least: what the VM was given
		nice   func(float64) float64
	}{
		{"CPU", cpu, float64(v.CPUs) * 100, func(x float64) float64 { return niceCeil(x/100) * 100 }},
		{"RAM", bytes, ram, niceBytes},
		{"Disk", bytes, 0, niceBytes},
	} {
		var vals [usageBuckets]float64
		values := make([]string, usageBuckets)
		peak, total, count := 0.0, 0.0, 0
		for i := range vals {
			if n[i] == 0 {
				continue
			}
			vals[i] = sum[k][i] / float64(n[i])
			values[i] = c.format(vals[i])
			peak = max(peak, vals[i])
			total += vals[i]
			count++
		}
		top := c.top
		if peak > top {
			top = c.nice(peak)
		}
		if top <= 0 {
			top = 1
		}
		line, area, dots := paths(vals[:], n[:], top)
		u.Charts = append(u.Charts, usageChart{Title: c.title, Top: c.format(top), Peak: c.format(peak),
			Avg: c.format(total / float64(count)), Line: line, Area: area, Dots: dots, Values: strings.Join(values, "|")})
	}
	return u
}

// paths draws vals (where n > 0) as a line and the area under it, with a
// gap where nothing was recorded, and a dot for a lone point; top is the
// value at the top.
func paths(vals []float64, n []int, top float64) (line, area, dots string) {
	var l, a, d strings.Builder
	x := func(i int) float64 { return (float64(i) + 0.5) * chartW / float64(len(vals)) }
	y := func(v float64) float64 { return chartH - min(v/top, 1)*chartH }
	for i := 0; i < len(vals); i++ {
		if n[i] == 0 {
			continue
		}
		j := i // the run of buckets with samples is i..j
		for j+1 < len(vals) && n[j+1] > 0 {
			j++
		}
		if i == j {
			fmt.Fprintf(&d, "M%.1f %.1fh0", x(i), y(vals[i])) // drawn by the round cap
			continue
		}
		fmt.Fprintf(&l, "M%.1f %.1f", x(i), y(vals[i]))
		fmt.Fprintf(&a, "M%.1f %d", x(i), chartH)
		for k := i; k <= j; k++ {
			if k > i {
				fmt.Fprintf(&l, "L%.1f %.1f", x(k), y(vals[k]))
			}
			fmt.Fprintf(&a, "L%.1f %.1f", x(k), y(vals[k]))
		}
		fmt.Fprintf(&a, "L%.1f %dZ", x(j), chartH)
		i = j
	}
	return l.String(), a.String(), d.String()
}

// niceCeil is the smallest of 1, 2, 5 times a power of ten that is >= x.
func niceCeil(x float64) float64 {
	if x <= 0 {
		return 1
	}
	p := math.Pow(10, math.Floor(math.Log10(x)))
	for _, m := range []float64{1, 2, 5, 10} {
		if m*p >= x {
			return m * p
		}
	}
	return 10 * p
}

// niceBytes is niceCeil in gigabytes, or megabytes under a gigabyte.
func niceBytes(x float64) float64 {
	if x >= 1<<30 {
		return niceCeil(x/(1<<30)) * (1 << 30)
	}
	return niceCeil(x/(1<<20)) * (1 << 20)
}
