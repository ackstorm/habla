package stats

import (
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"text/tabwriter"
	"time"

	"github.com/ackstorm/habla/internal/logs"
)

// typingWPM is the typing speed that time saved is measured against. ponytail: a constant, config when asked.
const typingWPM = 40

// Stdout is where the report goes. ponytail: a var only so tests can read it.
var Stdout io.Writer = os.Stdout

// Totals sums records; SavedMin and the means are filled in by done.
type Totals struct {
	Queries    int                `json:"queries"`
	Listen     int                `json:"listen"`
	Say        int                `json:"say"`
	Words      int                `json:"words"`
	AudioS     float64            `json:"audio_s"`
	Cost       float64            `json:"cost"`
	Service    map[string]float64 `json:"cost_by_service"`
	Unpriced   int                `json:"unpriced_calls"`
	SavedMin   float64            `json:"saved_min"`
	Mean       float64            `json:"mean_cost"`
	MeanListen float64            `json:"mean_cost_listen"`
	MeanSay    float64            `json:"mean_cost_say"`
	listenCost float64
	sayCost    float64
}

func (t *Totals) add(r Record) {
	if t.Service == nil {
		t.Service = map[string]float64{}
	}
	t.Queries++
	t.Words += r.Words
	t.AudioS += r.AudioS
	var cost float64
	for _, c := range r.Calls {
		cost += c.Cost
		t.Service[c.Service] += c.Cost
		if c.Source == "none" {
			t.Unpriced++
		}
	}
	t.Cost += cost
	if r.Kind == "say" {
		t.Say++
		t.sayCost += cost
	} else {
		t.Listen++
		t.listenCost += cost
		t.SavedMin += max(0, float64(r.Words)/typingWPM-r.AudioS/60)
	}
}

func (t *Totals) done() {
	if t.Service == nil {
		t.Service = map[string]float64{}
	}
	t.Mean, t.MeanListen, t.MeanSay = div(t.Cost, t.Queries), div(t.listenCost, t.Listen), div(t.sayCost, t.Say)
}

func div(sum float64, n int) float64 {
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

type Period struct {
	Name string `json:"name"` // today, week, month, total
	Totals
}

type Model struct {
	Service string  `json:"service"`
	Model   string  `json:"model"`
	Calls   int     `json:"calls"`
	In      int     `json:"in"`
	Audio   int     `json:"audio"`
	Out     int     `json:"out"`
	Cost    float64 `json:"cost"`
}

type Latency struct {
	Step string `json:"step"` // stt, transform, tts, total
	P50  int64  `json:"p50"`
	P95  int64  `json:"p95"`
	N    int    `json:"n"`
}

// Summary is the default report: calendar periods, then this month's models and step latencies.
type Summary struct {
	Periods []Period  `json:"periods"`
	Models  []Model   `json:"models"`
	Latency []Latency `json:"latency"`
}

// start is the local midnight that opens t's day, week (Monday) or month; the zero time for anything else (total).
func start(period string, t time.Time) time.Time {
	y, m, d := t.Date()
	switch period {
	case "day":
		return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
	case "week":
		return time.Date(y, m, d-(int(t.Weekday())+6)%7, 0, 0, 0, 0, t.Location())
	case "month":
		return time.Date(y, m, 1, 0, 0, 0, 0, t.Location())
	}
	return time.Time{}
}

func Summarize(records []Record, now time.Time) Summary {
	var s Summary
	for _, p := range [][2]string{{"today", "day"}, {"week", "week"}, {"month", "month"}, {"total", ""}} {
		period := Period{Name: p[0]}
		from := start(p[1], now)
		for _, r := range records {
			if !r.Time.Before(from) {
				period.add(r)
			}
		}
		period.done()
		s.Periods = append(s.Periods, period)
	}
	from := start("month", now)
	models := map[[2]string]*Model{}
	steps := map[string][]int64{}
	for _, r := range records {
		if r.Time.Before(from) {
			continue
		}
		for step, ms := range r.Ms {
			if ms > 0 {
				steps[step] = append(steps[step], ms)
			}
		}
		for _, c := range r.Calls {
			k := [2]string{c.Service, c.Model}
			m := models[k]
			if m == nil {
				m = &Model{Service: c.Service, Model: c.Model}
				models[k] = m
			}
			m.Calls++
			m.In, m.Audio, m.Out, m.Cost = m.In+c.In, m.Audio+c.Audio, m.Out+c.Out, m.Cost+c.Cost
			if c.Service == "tts" && c.Ms > 0 {
				steps["tts"] = append(steps["tts"], c.Ms)
			}
		}
	}
	for _, m := range models {
		s.Models = append(s.Models, *m)
	}
	slices.SortFunc(s.Models, func(a, b Model) int {
		return cmp.Or(cmp.Compare(a.Service, b.Service), cmp.Compare(a.Model, b.Model))
	})
	for _, step := range []string{"stt", "transform", "tts", "total"} {
		if v := steps[step]; len(v) > 0 {
			slices.Sort(v)
			s.Latency = append(s.Latency, Latency{step, percentile(v, 50), percentile(v, 95), len(v)})
		}
	}
	return s
}

// percentile is the nearest-rank p-th percentile of sorted, a non-empty ascending slice.
func percentile(sorted []int64, p int) int64 {
	return sorted[max((p*len(sorted)+99)/100-1, 0)]
}

// Row is one period of the --by history.
type Row struct {
	Start string `json:"start"` // 2026-10-18 (day), the Monday (week), 2026-10 (month)
	Totals
	from time.Time
}

// History is one row per period, newest first: the last 30 days, 12 weeks or 12 months.
func History(records []Record, by string, now time.Time) []Row {
	n, layout := 30, time.DateOnly
	switch by {
	case "week":
		n = 12
	case "month":
		n, layout = 12, "2006-01"
	}
	rows := make([]Row, n)
	from := start(by, now)
	for i := range rows {
		rows[i].from, rows[i].Start = from, from.Format(layout)
		switch by {
		case "day":
			from = from.AddDate(0, 0, -1)
		case "week":
			from = from.AddDate(0, 0, -7)
		default:
			from = from.AddDate(0, -1, 0)
		}
	}
	for _, r := range records {
		for i := range rows {
			if !r.Time.Before(rows[i].from) { // the newest period that started before r
				rows[i].add(r)
				break
			}
		}
	}
	for i := range rows {
		rows[i].done()
	}
	return rows
}

func Main(args []string) int {
	fs := flag.NewFlagSet("habla stats", flag.ContinueOnError)
	by := fs.String("by", "", "one row per `PERIOD`: day, week or month")
	asJSON := fs.Bool("json", false, "print JSON")
	logLevel := fs.String("log", "", "log at `LEVEL` (error, warn, info, debug) for this run")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 || !slices.Contains([]string{"", "day", "week", "month"}, *by) {
		fmt.Fprintln(os.Stderr, "usage: habla stats [--by day|week|month] [--json]")
		return 2
	}
	if err := logs.Setup("stats", *logLevel); err != nil {
		fmt.Fprintln(os.Stderr, "habla stats:", err)
		return 2
	}
	// ponytail: no config.Load: records are priced when written, so an invalid config never hides the stats
	records, err := Load(Path())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(os.Stderr, "habla stats:", err)
		return 1
	}
	if len(records) == 0 {
		fmt.Fprintln(Stdout, "No stats yet")
		return 0
	}
	now := time.Now()
	var report any
	if *by == "" {
		report = Summarize(records, now)
	} else {
		report = History(records, *by, now)
	}
	if *asJSON {
		e := json.NewEncoder(Stdout)
		e.SetIndent("", "  ")
		e.Encode(report)
		return 0
	}
	if *by == "" {
		printSummary(Stdout, report.(Summary))
	} else {
		printHistory(Stdout, report.([]Row))
	}
	return 0
}

func printSummary(w io.Writer, s Summary) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintf(tw, "%-16s\t", "")
	for _, p := range s.Periods {
		fmt.Fprintf(tw, "%s\t", p.Name)
	}
	fmt.Fprintln(tw)
	row := func(label, format string, value func(Totals) any) {
		fmt.Fprintf(tw, "%-16s\t", label) // padded: AlignRight would push the labels right
		for _, p := range s.Periods {
			fmt.Fprintf(tw, format+"\t", value(p.Totals))
		}
		fmt.Fprintln(tw)
	}
	row("queries", "%d", func(t Totals) any { return t.Queries })
	row("  listen", "%d", func(t Totals) any { return t.Listen })
	row("  say", "%d", func(t Totals) any { return t.Say })
	row("words", "%d", func(t Totals) any { return t.Words })
	row("audio min", "%.1f", func(t Totals) any { return t.AudioS / 60 })
	row("time saved min", "%.1f", func(t Totals) any { return t.SavedMin })
	row("cost USD", "%s", func(t Totals) any { return usd(t.Cost) })
	for _, svc := range []string{"stt", "llm", "tts"} {
		row("  "+svc, "%s", func(t Totals) any { return usd(t.Service[svc]) })
	}
	row("mean / query", "%s", func(t Totals) any { return usd(t.Mean) })
	row("  listen", "%s", func(t Totals) any { return usd(t.MeanListen) })
	row("  say", "%s", func(t Totals) any { return usd(t.MeanSay) })
	row("unpriced calls", "%d", func(t Totals) any { return t.Unpriced })
	tw.Flush()

	fmt.Fprintln(w)
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintf(tw, "%-40s\tcalls\tin tok\taudio tok\tout tok\tUSD\t\n", "model (this month)")
	for _, m := range s.Models {
		fmt.Fprintf(tw, "%-40s\t%d\t%d\t%d\t%d\t%s\t\n", m.Service+" "+m.Model, m.Calls, m.In, m.Audio, m.Out, usd(m.Cost))
	}
	fmt.Fprintf(tw, "\n%-40s\tp50\tp95\tn\t\n", "latency ms (this month)")
	for _, l := range s.Latency {
		fmt.Fprintf(tw, "%-40s\t%d\t%d\t%d\t\n", l.Step, l.P50, l.P95, l.N)
	}
	tw.Flush()
}

func printHistory(w io.Writer, rows []Row) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintf(tw, "%-10s\tqueries\twords\tUSD\tstt\tllm\ttts\tmean\t\n", "")
	for _, r := range rows {
		fmt.Fprintf(tw, "%-10s\t%d\t%d\t%s\t%s\t%s\t%s\t%s\t\n", r.Start, r.Queries, r.Words,
			usd(r.Cost), usd(r.Service["stt"]), usd(r.Service["llm"]), usd(r.Service["tts"]), usd(r.Mean))
	}
	tw.Flush()
}

// usd prints a cost so a small one stays visible: 4 decimals from $0.01 up, else 2 significant digits
// (0.000025, not 0.0000).
func usd(v float64) string {
	decimals := 4
	if v > 0 && v < 0.01 {
		decimals = 1 - int(math.Floor(math.Log10(v)))
	}
	return fmt.Sprintf("%.*f", decimals, v)
}
