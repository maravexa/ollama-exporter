package grafana_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type panel struct {
	ID                      int
	Type, Datasource, Title string
	Grid                    struct{ X, Y, W, H int } `json:"gridPos"`
	Targets                 []struct {
		Expr, RefID, Datasource, Format string
		Instant                         bool
		Range                           *bool
	}
	Options struct {
		TextMode, GraphMode string
		ShowHeader          bool
		ReduceOptions       struct{ Calcs []string }
	}
	FieldConfig struct {
		Defaults struct {
			NoValue string
			Custom  struct {
				Inspect  bool
				MinWidth int
			}
		}
		Overrides []json.RawMessage
	}
	Transformations []struct {
		ID      string
		Options map[string]json.RawMessage
	}
}

func TestDashboardOverview(t *testing.T) {
	data, err := os.ReadFile("dashboard.json")
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		SchemaVersion int
		UID           string
		Panels        []panel
		Templating    struct {
			List []struct{ Name, Type, Datasource string }
		}
	}
	if err = json.Unmarshal(data, &d); err != nil {
		t.Fatal(err)
	}
	if d.SchemaVersion != 38 || d.UID != "ollama-inference-v1" {
		t.Fatal("import schema/identity changed")
	}
	if len(d.Panels) != 13 {
		t.Fatalf("expected four totals, one table, eight history panels: got %d", len(d.Panels))
	}
	seen := map[int]bool{}
	stats, tables := 0, 0
	for i, p := range d.Panels {
		if seen[p.ID] {
			t.Errorf("duplicate panel %d", p.ID)
		}
		seen[p.ID] = true
		if p.Datasource != "${datasource}" {
			t.Errorf("panel %d bypasses selected datasource", p.ID)
		}
		a := p.Grid
		if a.X < 0 || a.Y < 0 || a.W <= 0 || a.H <= 0 || a.X+a.W > 24 {
			t.Errorf("invalid grid %d", p.ID)
		}
		for _, q := range d.Panels[i+1:] {
			b := q.Grid
			if a.X < b.X+b.W && b.X < a.X+a.W && a.Y < b.Y+b.H && b.Y < a.Y+a.H {
				t.Errorf("overlap %d/%d", p.ID, q.ID)
			}
		}
		refs := map[string]bool{}
		for _, q := range p.Targets {
			if q.Expr == "" || q.RefID == "" || refs[q.RefID] || q.Datasource != "${datasource}" {
				t.Errorf("invalid target in panel %d", p.ID)
			}
			refs[q.RefID] = true
			if p.Type == "table" {
				if !q.Instant || q.Range == nil || *q.Range {
					t.Errorf("panel %d can resurrect historical inventory", p.ID)
				}
			}
		}
		switch p.Type {
		case "stat":
			stats++
			if p.Title != "" || p.Options.GraphMode != "area" || len(p.Options.ReduceOptions.Calcs) != 1 || p.Options.ReduceOptions.Calcs[0] != "last" {
				t.Error("stats require no title, real sparklines and the latest sample including missing values")
			}
			for _, q := range p.Targets {
				if q.Instant || q.Range == nil || !*q.Range || !strings.HasSuffix(q.Expr, "or vector(0 / 0)") {
					t.Error("sparkline needs a range query with explicit unknown samples")
				}
			}
			for _, q := range p.Targets {
				if !strings.HasPrefix(q.Expr, "(sum(") && !strings.HasPrefix(q.Expr, "(count(") {
					t.Errorf("stat %d is not a total", p.ID)
				}
				if strings.Contains(q.Expr, "tokens_per_second") {
					t.Error("latest model throughput must not be summed")
				}
			}
		case "table":
			tables++
			if a.W != 24 || !p.Options.ShowHeader || !p.FieldConfig.Defaults.Custom.Inspect || p.FieldConfig.Defaults.Custom.MinWidth < 100 {
				t.Error("table must preserve full values with cell inspection and bounded column widths")
			}
			if p.FieldConfig.Defaults.NoValue != "Unknown" {
				t.Error("missing values must not become zero")
			}
			if len(p.Targets) != 5 {
				t.Error("missing model metric columns")
			}
			if len(p.Transformations) != 3 {
				t.Fatal("expected labels, full-label merge, organize transforms")
			}
			for j, want := range []string{"labelsToFields", "merge", "organize"} {
				if p.Transformations[j].ID != want {
					t.Errorf("transform %d: expected %s", j, want)
				}
			}
			if _, ok := p.Transformations[0].Options["keepLabels"]; ok {
				t.Error("must retain all scrape identity labels, including additional scope labels")
			}
			if string(p.Transformations[0].Options["valueLabel"]) != `"__dashboard_field__"` {
				t.Error("metric column pivot missing")
			}
			for _, q := range p.Targets {
				if q.Format != "time_series" || !strings.Contains(q.Expr, `ollama_model_loaded{job="ollama"}`) {
					t.Error("table must be inventory anchored time-series input")
				}
				if strings.Contains(q.Expr, " on (") || strings.Contains(q.Expr, " by (") {
					t.Error("do not drop extra scrape identity labels when joining")
				}
				if q.RefID != "A" && (!strings.Contains(q.Expr, " and ") || !strings.Contains(q.Expr, " * (0 / 0)")) {
					t.Error("optional metrics need inventory intersection and unknown fallback")
				}
			}
		case "timeseries":
		default:
			t.Errorf("unsupported panel %s", p.Type)
		}
	}
	if stats != 4 || tables != 1 {
		t.Errorf("wrong overview: %d totals, %d tables", stats, tables)
	}
	found := false
	for _, v := range d.Templating.List {
		if v.Name == "datasource" && v.Type == "datasource" {
			found = true
		}
		if v.Name == "endpoint" && v.Datasource != "${datasource}" {
			t.Error("endpoint variable bypasses datasource selection")
		}
	}
	if !found {
		t.Error("missing datasource picker")
	}
}

// Keep presentation choices independent of query values and identity joins.
func TestOverviewColorsAndFlexibleIdentityColumns(t *testing.T) {
	data, err := os.ReadFile("dashboard.json")
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Panels []struct {
			ID          int
			Title, Type string
			Options     struct{ ColorMode, TextMode string }
			Targets     []struct{ LegendFormat string }
			FieldConfig struct {
				Defaults  struct{ Mappings []json.RawMessage }
				Overrides []struct {
					Matcher    struct{ Options string }
					Properties []struct {
						ID    string
						Value json.RawMessage
					}
				}
			}
		}
	}
	if err = json.Unmarshal(data, &d); err != nil {
		t.Fatal(err)
	}
	checkUnknown := func(raw []json.RawMessage) {
		t.Helper()
		seen := map[string]bool{}
		for _, r := range raw {
			var m struct {
				Type    string
				Options struct {
					Match  string
					Result struct{ Text, Color string }
				}
			}
			if err := json.Unmarshal(r, &m); err != nil {
				t.Fatal(err)
			}
			if m.Type == "special" && (m.Options.Match == "nan" || m.Options.Match == "null") {
				seen[m.Options.Match] = true
				if m.Options.Result.Text != "Unknown" || m.Options.Result.Color != "#808080" {
					t.Error("unknown must have explicit neutral styling")
				}
			}
		}
		if !seen["nan"] || !seen["null"] {
			t.Error("both NaN and null need unknown styling")
		}
	}
	for _, p := range d.Panels {
		if p.Type == "stat" {
			if p.Options.ColorMode != "background" {
				t.Errorf("stat %d must color its background", p.ID)
			}
			checkUnknown(p.FieldConfig.Defaults.Mappings)
			if p.ID == 1 {
				if p.Title != "" || p.Options.TextMode != "value_and_name" || len(p.Targets) != 2 || p.Targets[0].LegendFormat != "Reachable" || p.Targets[1].LegendFormat != "Observed" {
					t.Error("reachability tile needs subtitles only")
				}
			}
		}
		if p.Type != "table" {
			continue
		}
		flexible := map[string]bool{}
		loaded := false
		for _, o := range p.FieldConfig.Overrides {
			props := map[string]json.RawMessage{}
			for _, v := range o.Properties {
				props[v.ID] = v.Value
			}
			if o.Matcher.Options == "Model" || o.Matcher.Options == "Exporter" {
				flexible[o.Matcher.Options] = true
				if _, ok := props["custom.width"]; ok {
					t.Error("identity columns must expand into available width")
				}
				var minimum int
				if err := json.Unmarshal(props["custom.minWidth"], &minimum); err != nil || minimum < 180 {
					t.Error("identity columns need a readable scrolling minimum")
				}
				if string(props["custom.inspect"]) != "true" {
					t.Error("full identity must remain inspectable")
				}
			}
			if o.Matcher.Options == "Loaded" {
				loaded = true
				var opts struct{ Type, Mode string }
				if err := json.Unmarshal(props["custom.cellOptions"], &opts); err != nil {
					t.Fatal(err)
				}
				if opts.Type != "color-background" || opts.Mode != "basic" {
					t.Error("loaded state needs a solid cell background")
				}
				var mappings []json.RawMessage
				if err := json.Unmarshal(props["mappings"], &mappings); err != nil {
					t.Fatal(err)
				}
				checkUnknown(mappings)
				var m struct {
					Options map[string]struct{ Text, Color string }
				}
				if err := json.Unmarshal(mappings[0], &m); err != nil {
					t.Fatal(err)
				}
				if m.Options["0"].Text != "Unloaded" || m.Options["0"].Color != "yellow" || m.Options["1"].Text != "Loaded" || m.Options["1"].Color != "green" {
					t.Error("loaded/unloaded values need explicit green/yellow mappings")
				}
			}
		}
		if !flexible["Model"] || !flexible["Exporter"] || !loaded {
			t.Error("missing identity/status overrides")
		}
	}
}

func TestNumericCellBands(t *testing.T) {
	data, err := os.ReadFile("dashboard.json")
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Panels []struct {
			Type        string
			FieldConfig struct {
				Overrides []struct {
					Matcher    struct{ Options string }
					Properties []struct {
						ID    string
						Value json.RawMessage
					}
				}
			}
		}
	}
	if err = json.Unmarshal(data, &d); err != nil {
		t.Fatal(err)
	}
	expected := map[string][]float64{"VRAM": {1, 4 * 1024 * 1024 * 1024, 16 * 1024 * 1024 * 1024}, "Decode · latest": {1, 10, 30}, "Prefill · latest": {1, 100, 500}, "Requests · 24h": {1, 100, 1000}}
	seen := map[string]bool{}
	for _, p := range d.Panels {
		if p.Type != "table" {
			continue
		}
		for _, o := range p.FieldConfig.Overrides {
			want, ok := expected[o.Matcher.Options]
			if !ok {
				continue
			}
			seen[o.Matcher.Options] = true
			props := map[string]json.RawMessage{}
			for _, v := range o.Properties {
				props[v.ID] = v.Value
			}
			var cell struct{ Type, Mode string }
			if err = json.Unmarshal(props["custom.cellOptions"], &cell); err != nil {
				t.Fatal(err)
			}
			if cell.Type != "color-background" || cell.Mode != "basic" {
				t.Error("numeric cells need solid threshold backgrounds")
			}
			var bands struct {
				Mode  string
				Steps []struct {
					Color string
					Value *float64
				}
			}
			if err = json.Unmarshal(props["thresholds"], &bands); err != nil {
				t.Fatal(err)
			}
			if bands.Mode != "absolute" || len(bands.Steps) != 4 {
				t.Fatal("expected absolute four-band scale")
			}
			if bands.Steps[0].Value != nil || bands.Steps[0].Color != "#808080" {
				t.Error("zero needs neutral base")
			}
			for i, value := range want {
				if bands.Steps[i+1].Value == nil || *bands.Steps[i+1].Value != value {
					t.Errorf("unexpected %s threshold", o.Matcher.Options)
				}
			}
			if o.Matcher.Options == "VRAM" || o.Matcher.Options == "Requests · 24h" {
				for _, b := range bands.Steps {
					if b.Color == "red" || b.Color == "yellow" || b.Color == "green" {
						t.Error("volume bands must not imply health")
					}
				}
			}
			var mappings []struct {
				Type    string
				Options struct {
					Match  string
					Result struct{ Text, Color string }
				}
			}
			if err = json.Unmarshal(props["mappings"], &mappings); err != nil {
				t.Fatal(err)
			}
			missing := map[string]bool{}
			for _, m := range mappings {
				missing[m.Options.Match] = m.Options.Result.Text == "Unknown" && m.Options.Result.Color == "#808080"
			}
			if !missing["nan"] || !missing["null"] {
				t.Error("missing numeric values must stay gray Unknown, not zero")
			}
		}
	}
	if len(seen) != 4 {
		t.Error("missing numeric color overrides")
	}
}
