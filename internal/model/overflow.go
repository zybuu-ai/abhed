package model

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// A provider that refuses a request for being longer than the model's window
// says so in its own words. These are the words, lower-cased, that mark such a
// refusal; each is specific enough not to match a rate limit, an output cap
// or a malformed request.
var overflowMarks = []string{
	"prompt is too long",                           // Anthropic, Bedrock and Vertex Claude
	"input is too long",                            // Bedrock
	"context_length_exceeded",                      // OpenAI's error code, and Groq's
	"maximum context length",                       // OpenAI, vLLM, DeepSeek, OpenRouter, Mistral
	"context length exceeded",                      //
	"exceeds the context window",                   // OpenAI Responses-style wording
	"exceeds the maximum number of tokens allowed", // Gemini
	"input token count",                            // Gemini: "The input token count (N) exceeds ..."
	"exceed_context_size_error",                    // llama.cpp
	"exceeds the available context size",           // llama.cpp
	"input length exceeds the context length",      // Ollama when it refuses rather than truncates
	"exceeds context length",                       // Ollama, TGI
	"too large for model with",                     // Mistral
	"input tokens exceed the configured limit",     // OpenAI
	"reduce the length of the messages",            // OpenAI-compatible servers
	"cannot exceed the total tokens limit",         // watsonx
	"input tokens. given",                          // TGI: "must have less than N input tokens. Given: M"
}

// limitPatterns pull the limit a refusal names, so the window can be set to
// what the server enforces. The first capture group is the limit.
var limitPatterns = []*regexp.Regexp{
	regexp.MustCompile(`tokens\s*>\s*([0-9][0-9,]*)\s*maximum`),                 // Anthropic
	regexp.MustCompile(`maximum context length is\s*([0-9][0-9,]*)`),            // OpenAI, vLLM, DeepSeek
	regexp.MustCompile(`maximum number of tokens allowed\s*\(([0-9][0-9,]*)\)`), // Gemini
	regexp.MustCompile(`configured limit of\s*([0-9][0-9,]*)`),                  // OpenAI
	regexp.MustCompile(`model with\s*([0-9][0-9,]*)\s*maximum context length`),  // Mistral
	regexp.MustCompile(`total tokens limit\s*([0-9][0-9,]*)`),                   // watsonx
	regexp.MustCompile(`context length (?:of|is)\s*([0-9][0-9,]*)`),             // Ollama
	regexp.MustCompile(`"n_ctx"\s*:\s*([0-9]+)`),                                // llama.cpp
	regexp.MustCompile(`less than\s*([0-9][0-9,]*)\s*input tokens`),             // TGI
	regexp.MustCompile(`context window of\s*([0-9][0-9,]*)`),                    //
}

// ContextOverflow reports whether err is a provider refusing a request as
// longer than the model's context window, and the limit it named, or zero
// when it named none. Only a refusal is matched: a server error, a timeout or
// a rate limit is not, whatever its text.
func ContextOverflow(err error) (overflow bool, limit int) {
	if err == nil {
		return false, 0
	}
	var se *StatusError
	if errors.As(err, &se) && se.Status != 400 && se.Status != 413 && se.Status != 422 {
		return false, 0
	}
	var te *TimeoutError
	if errors.As(err, &te) {
		return false, 0
	}
	text := strings.ToLower(err.Error())
	found := false
	for _, m := range overflowMarks {
		if strings.Contains(text, m) {
			found = true
			break
		}
	}
	if !found {
		return false, 0
	}
	for _, re := range limitPatterns {
		if m := re.FindStringSubmatch(text); m != nil {
			if n, err := strconv.Atoi(strings.ReplaceAll(m[1], ",", "")); err == nil && n > 0 {
				return true, n
			}
		}
	}
	return true, 0
}

// TruncatedPrompt reports a prompt a server cut to fit its window rather than
// refusing it, as Ollama does: the prompt it says it read is well short of
// what was sent. sent is the harness's own estimate and read the provider's
// count; a small prompt is never judged, since the estimate is approximate.
func TruncatedPrompt(p Profile, sent, read int) bool {
	if !p.SilentTruncation || read <= 0 || sent < 4096 {
		return false
	}
	// A third short is far beyond the estimate's error, and a cut prompt
	// arrives at almost exactly the server's window.
	return read*3 < sent*2
}
