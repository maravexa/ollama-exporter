// Package ollama provides a typed client and response types for the Ollama REST API.
package ollama

import "time"

// PSResponse is the response from GET /api/ps.
// It lists models currently loaded in VRAM.
type PSResponse struct {
	Models []RunningModel `json:"models"`
}

// RunningModel describes a model currently resident in VRAM.
type RunningModel struct {
	Name      string    `json:"name"`
	Model     string    `json:"model"`
	Size      int64     `json:"size"`
	Digest    string    `json:"digest"`
	ExpiresAt time.Time `json:"expires_at"`
	SizeVRAM  int64     `json:"size_vram"`
}

// TagsResponse is the response from GET /api/tags.
// It lists all locally pulled models.
type TagsResponse struct {
	Models []LocalModel `json:"models"`
}

// LocalModel describes a locally available model.
type LocalModel struct {
	Name       string    `json:"name"`
	Model      string    `json:"model"`
	ModifiedAt time.Time `json:"modified_at"`
	Size       int64     `json:"size"`
	Digest     string    `json:"digest"`
}

// ShowResponse is the response from POST /api/show.
type ShowResponse struct {
	Details ModelDetails `json:"details"`
}

// ModelDetails contains metadata about a model returned by /api/show.
type ModelDetails struct {
	QuantizationLevel string `json:"quantization_level"`
	Family            string `json:"family"`
	ParameterSize     string `json:"parameter_size"`
}

// GenerateResponse is the final (done=true) chunk from /api/generate or /api/chat.
// Timing fields are in nanoseconds.
type GenerateResponse struct {
	Model              string `json:"model"`
	Done               bool   `json:"done"`
	TotalDuration      int64  `json:"total_duration"`
	LoadDuration       int64  `json:"load_duration"`
	PromptEvalCount    int64  `json:"prompt_eval_count"`
	PromptEvalDuration int64  `json:"prompt_eval_duration"`
	EvalCount          int64  `json:"eval_count"`
	EvalDuration       int64  `json:"eval_duration"`
}

// OpenAIUsage mirrors the usage object returned by Ollama's /v1/* endpoints.
// Timing fields are in milliseconds (not nanoseconds).
type OpenAIUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	// TotalTime, PromptTime, CompletionTime are in milliseconds.
	TotalTime      int64 `json:"total_time,omitempty"`
	PromptTime     int64 `json:"prompt_time,omitempty"`
	CompletionTime int64 `json:"completion_time,omitempty"`
}

// OpenAIChatCompletionResponse mirrors the structure returned by Ollama's /v1/chat/completions endpoint.
type OpenAIChatCompletionResponse struct {
	Usage OpenAIUsage `json:"usage"`
}
