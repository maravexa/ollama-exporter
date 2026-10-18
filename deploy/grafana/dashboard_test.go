package grafana_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Stat panels must identify each model/exporter without falling back to the
// complete Prometheus label set (issue #21). Instant queries also prevent the
// lastNotNull reducer from resurrecting removed series from the dashboard range.
func TestOverviewStatsUseNamedCurrentSeries(t *testing.T) {
	data, err := os.ReadFile("dashboard.json")
	if err != nil {
		t.Fatal(err)
	}
	var dashboard struct {
		Panels []struct {
			Title   string `json:"title"`
			Type    string `json:"type"`
			Options struct {
				TextMode string `json:"textMode"`
			} `json:"options"`
			Targets []struct {
				Expr         string `json:"expr"`
				LegendFormat string `json:"legendFormat"`
				Instant      bool   `json:"instant"`
				Range        *bool  `json:"range"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(data, &dashboard); err != nil {
		t.Fatal(err)
	}
	stats := 0
	for _, panel := range dashboard.Panels {
		if panel.Type != "stat" {
			continue
		}
		stats++
		t.Run(panel.Title, func(t *testing.T) {
			if panel.Options.TextMode != "value_and_name" {
				t.Error("stat must show the value and a concise series name, including for one model")
			}
			if len(panel.Targets) == 0 {
				t.Fatal("stat has no query")
			}
			for _, target := range panel.Targets {
				if !target.Instant || target.Range == nil || *target.Range {
					t.Error("overview must use only an instant query; a range plus lastNotNull retains historical series")
				}
				if !strings.Contains(target.LegendFormat, "{{instance}}") {
					t.Error("legend must identify the exporter instance instead of falling back to raw labels")
				}
				if target.Expr != "ollama_up" && !strings.Contains(target.LegendFormat, "{{model}}") {
					t.Error("per-model stat legend must identify the model")
				}
				if strings.Contains(target.Expr, "ollama_requests_total") && !strings.Contains(target.LegendFormat, "{{endpoint}}") {
					t.Error("request stat legend must distinguish endpoints")
				}
			}
		})
	}
	if stats != 6 {
		t.Errorf("expected six overview stats, got %d", stats)
	}
}

// This checks the shipped import structure and overview query semantics, not a
// Grafana rendering or full validation against Grafana's versioned JSON schema.
func TestDashboardImportStructureAndOverviewQueries(t *testing.T) {
	data, err := os.ReadFile("dashboard.json")
	if err != nil {
		t.Fatal(err)
	}
	var dashboard struct {
		SchemaVersion int                                     `json:"schemaVersion"`
		UID           string                                  `json:"uid"`
		Inputs        []struct{ Name, Type, PluginID string } `json:"__inputs"`
		Panels        []struct {
			ID          int    `json:"id"`
			Type        string `json:"type"`
			Datasource  string `json:"datasource"`
			FieldConfig struct {
				Defaults struct {
					Unit string `json:"unit"`
				} `json:"defaults"`
			} `json:"fieldConfig"`
			Targets []struct{ Expr, RefID, Datasource, LegendFormat string } `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(data, &dashboard); err != nil {
		t.Fatal(err)
	}
	if dashboard.SchemaVersion != 38 || dashboard.UID == "" {
		t.Error("expected schemaVersion 38 and stable nonempty dashboard UID")
	}
	if len(dashboard.Inputs) != 1 || dashboard.Inputs[0].Name != "DS_PROMETHEUS" || dashboard.Inputs[0].PluginID != "prometheus" || dashboard.Inputs[0].Type != "datasource" {
		t.Error("missing Prometheus import datasource input")
	}
	expected := map[int]struct{ expr, unit string }{
		1: {`ollama_up`, ""},
		2: {`ollama_model_loaded{job="ollama"}`, ""},
		3: {`ollama_model_vram_bytes{job="ollama"}`, "bytes"},
		4: {`ollama_tokens_per_second{job="ollama"}`, "short"},
		5: {`ollama_prompt_tokens_per_second{job="ollama"}`, "short"},
		6: {`increase(ollama_requests_total{job="ollama"}[24h])`, "short"},
	}
	seen := map[int]bool{}
	for _, panel := range dashboard.Panels {
		if panel.ID <= 0 || seen[panel.ID] {
			t.Errorf("invalid or duplicate panel ID %d", panel.ID)
		}
		seen[panel.ID] = true
		if panel.Datasource != "${datasource}" {
			t.Errorf("panel %d does not use selected datasource", panel.ID)
		}
		if panel.Type != "stat" && panel.Type != "timeseries" {
			t.Errorf("panel %d has unsupported type %q", panel.ID, panel.Type)
		}
		refs := map[string]bool{}
		for _, target := range panel.Targets {
			if target.RefID == "" || refs[target.RefID] {
				t.Errorf("panel %d has empty/duplicate query refID", panel.ID)
			}
			refs[target.RefID] = true
			if target.Expr == "" || target.Datasource != "${datasource}" {
				t.Errorf("panel %d has missing query or incorrect datasource", panel.ID)
			}
		}
		if want, ok := expected[panel.ID]; ok {
			if panel.Type != "stat" || len(panel.Targets) != 1 {
				t.Errorf("overview panel %d must have one stat query", panel.ID)
				continue
			}
			if panel.Targets[0].Expr != want.expr {
				t.Errorf("panel %d query changed: got %q, want %q", panel.ID, panel.Targets[0].Expr, want.expr)
			}
			if panel.FieldConfig.Defaults.Unit != want.unit {
				t.Errorf("panel %d has incorrect numeric unit", panel.ID)
			}
			if panel.ID > 1 && !strings.Contains(panel.Targets[0].LegendFormat, "{{quant}}") {
				t.Errorf("panel %d legend loses quantization identity", panel.ID)
			}
		}
	}
	for id := range expected {
		if !seen[id] {
			t.Errorf("missing overview panel %d", id)
		}
	}
	if len(dashboard.Panels) != 14 {
		t.Errorf("expected 14 panels, got %d", len(dashboard.Panels))
	}
}

func TestOverviewLayoutPreservesReadableLabels(t *testing.T) {
	data, err := os.ReadFile("dashboard.json")
	if err != nil {
		t.Fatal(err)
	}
	var dashboard struct {
		Panels []struct {
			ID      int                      `json:"id"`
			Type    string                   `json:"type"`
			Grid    struct{ X, Y, W, H int } `json:"gridPos"`
			Options struct {
				Text struct{ TitleSize, ValueSize int } `json:"text"`
			} `json:"options"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(data, &dashboard); err != nil {
		t.Fatal(err)
	}
	for i, p := range dashboard.Panels {
		a := p.Grid
		if a.X < 0 || a.Y < 0 || a.W <= 0 || a.H <= 0 || a.X+a.W > 24 {
			t.Errorf("panel %d has invalid grid bounds", p.ID)
		}
		if p.Type == "stat" {
			// Three model series must have room for identity labels as well as values.
			if a.W < 8 || a.H < 4 {
				t.Errorf("overview panel %d is too narrow/short for multiple named series", p.ID)
			}
			if p.Options.Text.TitleSize < 12 || p.Options.Text.ValueSize < 24 {
				t.Errorf("overview panel %d needs explicit readable label/value sizes", p.ID)
			}
		}
		for _, q := range dashboard.Panels[i+1:] {
			b := q.Grid
			if a.X < b.X+b.W && b.X < a.X+a.W && a.Y < b.Y+b.H && b.Y < a.Y+a.H {
				t.Errorf("panels %d and %d overlap", p.ID, q.ID)
			}
		}
	}
}
