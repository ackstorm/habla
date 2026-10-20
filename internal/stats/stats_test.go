package stats

import (
	"encoding/json"
	"math"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ackstorm/habla/internal/config"
	"github.com/ackstorm/habla/internal/gateway"
)

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
	if err != nil {
		panic(err)
	}
	return t
}

var now = at("2026-10-18 12:00") // a Sunday

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func sample() []Record {
	return []Record{
		{Time: at("2026-10-18 09:00"), Kind: "listen", AudioS: 6, Words: 30, Ms: map[string]int64{"stt": 600, "transform": 400, "total": 1100},
			Calls: []Call{{Service: "stt", Model: "m-stt", Audio: 100, Out: 8, Cost: 0.002, Source: "header"},
				{Service: "llm", Model: "m-llm", In: 600, Out: 50, Cost: 0.001, Source: "prices"}}},
		{Time: at("2026-10-12 08:00"), Kind: "say", Words: 10, // Monday: this week, not today
			Calls: []Call{{Service: "tts", Model: "m-tts", Ms: 900, Cost: 0.004, Source: "header"}}},
		{Time: at("2026-10-11 23:00"), Kind: "listen", AudioS: 3, Words: 12, Ms: map[string]int64{"stt": 800, "total": 900}, // last week
			Calls: []Call{{Service: "stt", Model: "m-stt", Source: "none"}}},
		{Time: at("2026-09-30 10:00"), Kind: "listen", Words: 5, // last month
			Calls: []Call{{Service: "stt", Model: "m-stt", Cost: 0.001, Source: "header"}}},
	}
}

func TestSummarizeCalendarPeriods(t *testing.T) {
	s := Summarize(sample(), now)
	var queries []int
	for _, p := range s.Periods {
		queries = append(queries, p.Queries)
	}
	if !slices.Equal(queries, []int{1, 2, 3, 4}) {
		t.Fatal(queries)
	}
	week := s.Periods[1]
	if week.Listen != 1 || week.Say != 1 || !near(week.Cost, 0.007) || !near(week.Service["tts"], 0.004) ||
		!near(week.Mean, 0.0035) || !near(week.MeanListen, 0.003) || !near(week.MeanSay, 0.004) {
		t.Fatalf("%+v", week)
	}
	if month := s.Periods[2]; month.Unpriced != 1 {
		t.Fatal(month.Unpriced)
	}
	if today := s.Periods[0]; !near(today.SavedMin, 0.65) { // 30 words at 40 wpm = 0.75 min, minus 6 s spoken
		t.Fatal(today.SavedMin)
	}
}

func TestSummarizeModelsAndLatencyThisMonth(t *testing.T) {
	s := Summarize(sample(), now)
	models := []Model{ // the September call is outside the month
		{Service: "llm", Model: "m-llm", Calls: 1, In: 600, Out: 50, Cost: 0.001},
		{Service: "stt", Model: "m-stt", Calls: 2, Audio: 100, Out: 8, Cost: 0.002},
		{Service: "tts", Model: "m-tts", Calls: 1, Cost: 0.004},
	}
	if !reflect.DeepEqual(s.Models, models) {
		t.Fatalf("%+v", s.Models)
	}
	latency := []Latency{{"stt", 600, 800, 2}, {"transform", 400, 400, 1}, {"tts", 900, 900, 1}, {"total", 900, 1100, 2}}
	if !reflect.DeepEqual(s.Latency, latency) {
		t.Fatalf("%+v", s.Latency)
	}
}

func TestPercentileIsNearestRank(t *testing.T) {
	var v []int64
	for i := int64(1); i <= 20; i++ {
		v = append(v, i)
	}
	if percentile(v, 50) != 10 || percentile(v, 95) != 19 || percentile([]int64{7}, 95) != 7 {
		t.Fatal(percentile(v, 50), percentile(v, 95))
	}
}

func TestHistoryWeeksStartOnMonday(t *testing.T) {
	rows := History(sample(), "week", now)
	if len(rows) != 12 {
		t.Fatal(len(rows))
	}
	if rows[3].Service == nil {
		t.Fatal("an empty row has a nil Service map")
	}
	for i, want := range []struct {
		start   string
		queries int
	}{{"2026-10-12", 2}, {"2026-10-05", 1}, {"2026-09-28", 1}, {"2026-09-21", 0}} {
		if rows[i].Start != want.start || rows[i].Queries != want.queries {
			t.Fatalf("row %d: %+v", i, rows[i])
		}
	}
}

func TestHistoryByMonth(t *testing.T) {
	rows := History(sample(), "month", now)
	if len(rows) != 12 || rows[0].Start != "2026-10" || rows[0].Queries != 3 || rows[1].Start != "2026-09" || rows[1].Queries != 1 {
		t.Fatalf("%+v", rows[:2])
	}
}

func TestCallsPricing(t *testing.T) {
	got := Calls([]gateway.Call{
		{Service: "stt", Model: "m", Audio: 1000, Out: 10, Cost: 0.5, Costed: true}, // the gateway's cost wins
		{Service: "llm", Model: "m", In: 1000, Out: 10},                             // (1000×1 + 10×100) / 1e6
		{Service: "llm", Model: "other", In: 5},                                     // no price
		{Service: "tts", Model: "m"},                                                // nothing to price
	}, map[string]config.Price{"m": {Input: 1, Audio: 10, Output: 100}})
	var sources []string
	for _, c := range got {
		sources = append(sources, c.Source)
	}
	if !slices.Equal(sources, []string{"header", "prices", "none", "none"}) || got[0].Cost != 0.5 || !near(got[1].Cost, 0.002) {
		t.Fatalf("%+v", got)
	}
}

func TestAppendAndLoadSkipMalformedLines(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	Append(sample()[0])
	Append(sample()[1])
	f, err := os.OpenFile(Path(), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("not json\n")
	f.Close()
	records, err := Load(Path())
	if err != nil || len(records) != 2 || records[1].Kind != "say" {
		t.Fatal(records, err)
	}
	if info, _ := os.Stat(Path()); info.Mode().Perm() != 0o600 {
		t.Fatal(info.Mode())
	}
}

func TestMainReports(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var out strings.Builder
	saved := Stdout
	Stdout = &out
	t.Cleanup(func() { Stdout = saved })
	if code := Main(nil); code != 0 || out.String() != "No stats yet\n" {
		t.Fatal(code, out.String())
	}
	r := sample()[0]
	r.Time = time.Now()
	Append(r)
	out.Reset()
	if code := Main(nil); code != 0 || !strings.Contains(out.String(), "mean / query") || !strings.Contains(out.String(), "stt m-stt") {
		t.Fatal(code, out.String())
	}
	out.Reset()
	if code := Main([]string{"--by", "day", "--json"}); code != 0 {
		t.Fatal(code)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out.String()), &rows); err != nil || len(rows) != 30 || rows[0]["queries"] != 1.0 {
		t.Fatal(err, out.String())
	}
	if code := Main([]string{"--by", "year"}); code != 2 {
		t.Fatal(code)
	}
}

func TestUSDKeepsSmallCostsVisible(t *testing.T) {
	for v, want := range map[float64]string{0: "0.0000", 4.7123: "4.7123", 0.01: "0.0100", 0.0058: "0.0058",
		0.00059: "0.00059", 0.000025: "0.000025", 0.0000254: "0.000025"} {
		if got := usd(v); got != want {
			t.Errorf("usd(%v) = %q, want %q", v, got, want)
		}
	}
}
