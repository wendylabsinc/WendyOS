package chat

import "context"

// TokenUsage describes one model request. InputTokens includes cache reads and
// writes; reasoning tokens are a subset of OutputTokens. Complete is false when
// the provider omitted usage or the request failed before its final usage report.
type TokenUsage struct {
	Provider          string `json:"provider"`
	Model             string `json:"model"`
	InputTokens       int64  `json:"input_tokens"`
	CachedInputTokens int64  `json:"cached_input_tokens"`
	CacheWriteTokens  int64  `json:"cache_write_tokens"`
	OutputTokens      int64  `json:"output_tokens"`
	ReasoningTokens   int64  `json:"reasoning_tokens"`
	Complete          bool   `json:"complete"`
}

type usageKey struct{}

// wireUsage accepts both Responses and Chat Completions names. Pointers keep an
// omitted count distinct from a reported zero.
type wireUsage struct {
	Input        *int64 `json:"input_tokens"`
	Prompt       *int64 `json:"prompt_tokens"`
	Output       *int64 `json:"output_tokens"`
	Completion   *int64 `json:"completion_tokens"`
	CacheRead    int64  `json:"cache_read_input_tokens"`
	CacheWrite   int64  `json:"cache_creation_input_tokens"`
	InputDetails struct {
		Cached int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	PromptDetails struct {
		Cached int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	OutputDetails struct {
		Reasoning int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
	CompletionDetails struct {
		Reasoning int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func recordUsage(ctx context.Context, wire *wireUsage, anthropic bool) {
	u, _ := ctx.Value(usageKey{}).(*TokenUsage)
	if u == nil || wire == nil {
		return
	}
	in, out := wire.Input, wire.Output
	if in == nil {
		in = wire.Prompt
	}
	if out == nil {
		out = wire.Completion
	}
	if in != nil {
		u.InputTokens = *in
		u.CachedInputTokens = wire.InputDetails.Cached + wire.PromptDetails.Cached
		u.CacheWriteTokens = wire.CacheWrite
		if anthropic {
			u.CachedInputTokens = wire.CacheRead
			u.InputTokens += wire.CacheRead + wire.CacheWrite
		}
	}
	if out != nil {
		u.OutputTokens = *out
	}
	u.ReasoningTokens = wire.OutputDetails.Reasoning + wire.CompletionDetails.Reasoning
	// Anthropic message_delta only contains output usage. The caller merges it
	// with message_start before recording the final snapshot.
	u.Complete = in != nil && out != nil
}
