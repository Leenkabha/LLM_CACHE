package llm

import "testing"

func TestExtractResponseTextUsesOutputText(t *testing.T) {
	got := extractResponseText(responsesResponse{OutputText: " hello "})
	if got != "hello" {
		t.Fatalf("extractResponseText() = %q, want hello", got)
	}
}

func TestExtractResponseTextFallsBackToOutputContent(t *testing.T) {
	resp := responsesResponse{
		Output: []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}{
			{Content: []struct {
				Text string `json:"text"`
			}{{Text: "first"}, {Text: "second"}}},
		},
	}
	got := extractResponseText(resp)
	if got != "first\nsecond" {
		t.Fatalf("extractResponseText() = %q, want joined content text", got)
	}
}
