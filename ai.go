package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	anthropicURL     = "https://api.anthropic.com/v1/messages"
	anthropicVersion = "2023-06-01"
	nlModel          = "claude-haiku-4-5"
)

const nlSystemPrompt = `You translate plain-english descriptions into log filter expressions.

Filter syntax — terms are separated by spaces and ALL must match:
  word           line contains "word" anywhere
  field=value    JSON field contains value (equal numbers match exactly)
  field>n        numeric comparison; also >= < <=

There is no OR and no quoting. Levels are filtered with e.g. level=error.
Prefer field terms over bare words when a field clearly fits.
Reply with ONLY the filter expression on a single line, nothing else.`

// nlToFilter asks Claude to turn a plain-english request into a filter
// expression, given the fields (and sample values) seen in this stream.
func nlToFilter(query string, fields []string, samples map[string][]string) (string, error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return "", fmt.Errorf("set ANTHROPIC_API_KEY for plain-english search")
	}

	var ctx strings.Builder
	ctx.WriteString("Fields seen in this log stream, with example values:\n")
	for _, f := range fields {
		fmt.Fprintf(&ctx, "  %s: %s\n", f, strings.Join(samples[f], ", "))
	}
	if len(fields) == 0 {
		ctx.WriteString("  (no JSON fields seen yet)\n")
	}

	body, err := json.Marshal(map[string]any{
		"model":      nlModel,
		"max_tokens": 200,
		"system":     nlSystemPrompt + "\n\n" + ctx.String(),
		"messages": []map[string]string{
			{"role": "user", "content": query},
		},
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest(http.MethodPost, anthropicURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("content-type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Error != nil {
		return "", fmt.Errorf("claude: %s", out.Error.Message)
	}
	if len(out.Content) == 0 {
		return "", fmt.Errorf("claude returned nothing")
	}
	return cleanFilter(out.Content[0].Text), nil
}

// cleanFilter strips the wrapping a model sometimes adds (fences, quotes)
// and keeps the first non-empty line.
func cleanFilter(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.Trim(strings.TrimSpace(line), "`\"'")
		if line != "" && !strings.HasPrefix(line, "```") {
			return line
		}
	}
	return ""
}
