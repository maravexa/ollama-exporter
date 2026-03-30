package collector

import (
	"context"
	"strings"
	"sync"

	"github.com/maravexa/ollama-exporter/internal/ollama"
)

// ModelInfo holds the resolved family and quantization for a model.
type ModelInfo struct {
	Family string
	Quant  string
}

// ModelCache is a thread-safe, write-once cache of ModelInfo keyed by model name.
// It calls /api/show on the first access for each unique model name.
type ModelCache struct {
	mu     sync.RWMutex
	cache  map[string]ModelInfo
	client *ollama.Client
}

// NewModelCache constructs a ModelCache backed by the given client.
func NewModelCache(client *ollama.Client) *ModelCache {
	return &ModelCache{
		cache:  make(map[string]ModelInfo),
		client: client,
	}
}

// Get returns ModelInfo for the given model name, fetching from /api/show on
// first access. On any error it falls back to parseModelName.
func (mc *ModelCache) Get(ctx context.Context, model string) ModelInfo {
	mc.mu.RLock()
	if info, ok := mc.cache[model]; ok {
		mc.mu.RUnlock()
		return info
	}
	mc.mu.RUnlock()

	// Cache miss — fetch from Ollama.
	info := mc.fetchAndCache(ctx, model)
	return info
}

func (mc *ModelCache) fetchAndCache(ctx context.Context, model string) ModelInfo {
	// Re-check under write lock to avoid duplicate fetches.
	mc.mu.Lock()
	defer mc.mu.Unlock()
	if info, ok := mc.cache[model]; ok {
		return info
	}

	info := mc.fetch(ctx, model)
	mc.cache[model] = info
	return info
}

func (mc *ModelCache) fetch(ctx context.Context, model string) ModelInfo {
	resp, err := mc.client.Show(ctx, model)
	if err != nil {
		family, _ := parseModelName(model)
		return ModelInfo{Family: family, Quant: "unknown"}
	}

	family := strings.ToLower(strings.TrimSpace(resp.Details.Family))
	quant := strings.ToLower(strings.TrimSpace(resp.Details.QuantizationLevel))

	if family == "" {
		family, _ = parseModelName(model)
	}
	if quant == "" {
		quant = "unknown"
	}

	return ModelInfo{Family: family, Quant: quant}
}

// parseModelName extracts family and quantization labels from an Ollama model tag.
// e.g. "llama3.1:8b-q4_0" → family="llama3", quant="q4_0"
// Used as a fallback when /api/show is unavailable.
//
//nolint:unparam // quant used in fallback ModelInfo, future callers may use it
func parseModelName(name string) (family, quant string) {
	family = "unknown"
	quant = "unknown"

	parts := strings.SplitN(name, ":", 2)
	if len(parts) > 0 {
		base := parts[0]
		// Strip version suffixes like ".1", ".2" for family grouping.
		if idx := strings.Index(base, "."); idx > 0 {
			base = base[:idx]
		}
		family = base
	}

	if len(parts) == 2 {
		tag := parts[1]
		// Quantization appears after the last "-" in the tag.
		if idx := strings.LastIndex(tag, "-"); idx >= 0 {
			quant = tag[idx+1:]
		} else {
			quant = tag
		}
	}

	return family, quant
}
