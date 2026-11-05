package adapter

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"slices"
	"strings"
)

// LastAssistantFromTranscript returns the assistant text of the last turn from a Claude Code
// style JSONL transcript, for runtimes whose Stop hook does not carry the message itself.
func LastAssistantFromTranscript(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	const tailSize = 1 << 20
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	off := max(0, st.Size()-tailSize)
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return ""
	}
	lines := bytes.Split(buf, []byte("\n"))
	var parts []string
	for _, line := range slices.Backward(lines) {
		var rec struct {
			Type    string `json:"type"`
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		switch rec.Type {
		case "assistant":
			var blocks []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(rec.Message.Content, &blocks) == nil {
				for _, b := range slices.Backward(blocks) {
					if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
						parts = append(parts, b.Text)
					}
				}
			}
		case "user":
			if isRealPrompt(rec.Message.Content) {
				slices.Reverse(parts)
				return strings.Join(parts, "\n")
			}
		}
	}
	slices.Reverse(parts)
	return strings.Join(parts, "\n")
}

// isRealPrompt tells a typed user prompt apart from tool_result carrier messages.
func isRealPrompt(content json.RawMessage) bool {
	content = bytes.TrimSpace(content)
	if len(content) > 0 && content[0] == '"' {
		return true
	}
	var blocks []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return false
	}
	for _, b := range blocks {
		if b.Type == "tool_result" {
			return false
		}
	}
	return len(blocks) > 0
}
