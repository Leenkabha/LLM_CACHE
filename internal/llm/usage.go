package llm

import (
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // the quota day is Pacific time; minimal container images ship no tz database
)

// geminiUsage is the token accounting Gemini returns with every answer.
type geminiUsage struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	ThoughtsTokenCount   int `json:"thoughtsTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

// usageTracker keeps a running per-day tally of Gemini calls and tokens and
// logs how much is left. Gemini does not report remaining quota, so "left" is
// an estimate: the configured limit minus what this process has counted since
// Google's quota day began (midnight Pacific). The tally lives in memory, so a
// restart begins the day's count again; a real 429 always corrects the log.
//
// A nil *usageTracker is valid and does nothing.
type usageTracker struct {
	mu           sync.Mutex
	loc          *time.Location
	now          func() time.Time
	day          string // Pacific calendar date the counters belong to
	requests     int
	tokens       int
	exhausted    bool
	requestLimit int // 0 = unknown
	tokenBudget  int // 0 = unlimited/unknown
}

func newUsageTracker(requestLimit, tokenBudget int) *usageTracker {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		loc = time.UTC
	}
	return &usageTracker{loc: loc, now: time.Now, requestLimit: requestLimit, tokenBudget: tokenBudget}
}

// rollover starts a fresh tally when the Pacific date has changed. Callers hold mu.
func (t *usageTracker) rollover() (resetIn time.Duration) {
	now := t.now().In(t.loc)
	if day := now.Format("2006-01-02"); day != t.day {
		t.day, t.requests, t.tokens, t.exhausted = day, 0, 0, false
	}
	next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, t.loc)
	return next.Sub(now)
}

// recordSuccess counts one answered request and logs its tokens plus the
// running totals and what is left.
func (t *usageTracker) recordSuccess(model string, u geminiUsage) {
	if t == nil {
		return
	}
	t.mu.Lock()
	resetIn := t.rollover()
	total := u.TotalTokenCount
	if total == 0 {
		total = u.PromptTokenCount + u.CandidatesTokenCount + u.ThoughtsTokenCount
	}
	t.requests++
	t.tokens += total
	line := fmt.Sprintf("gemini usage: model=%s prompt_tokens=%d output_tokens=%d thinking_tokens=%d total_tokens=%d | today: requests=%d tokens=%d | %s | resets_in=%s",
		model, u.PromptTokenCount, u.CandidatesTokenCount, u.ThoughtsTokenCount, total,
		t.requests, t.tokens, t.leftLocked(), resetIn.Round(time.Minute))
	t.mu.Unlock()
	log.Print(line)
}

var quotaLimitRe = regexp.MustCompile(`limit: (\d+)`)

// recordQuotaExhausted logs a per-day quota 429 from Google. It also learns the
// daily request limit from the error text when none was configured, so
// "requests_left" is accurate from then on.
func (t *usageTracker) recordQuotaExhausted(model string, body []byte) {
	if t == nil {
		return
	}
	t.mu.Lock()
	resetIn := t.rollover()
	t.exhausted = true
	if m := quotaLimitRe.FindSubmatch(body); m != nil && t.requestLimit == 0 {
		if n, err := strconv.Atoi(string(m[1])); err == nil {
			t.requestLimit = n
		}
	}
	line := fmt.Sprintf("gemini quota EXHAUSTED: model=%s | today: requests=%d tokens=%d | %s | resets_in=%s (Google's daily quota resets at midnight Pacific; enable billing or switch GEMINI_MODEL)",
		model, t.requests, t.tokens, t.leftLocked(), resetIn.Round(time.Minute))
	t.mu.Unlock()
	log.Print(line)
}

// leftLocked renders the remaining requests/tokens. Callers hold mu.
func (t *usageTracker) leftLocked() string {
	reqs, toks := "requests_left=unknown(set GEMINI_DAILY_REQUEST_LIMIT)", "tokens_left=unlimited"
	low := false
	if t.exhausted {
		reqs = "requests_left=0"
		if t.requestLimit > 0 {
			reqs = fmt.Sprintf("requests_left=0/%d", t.requestLimit)
		}
		low = true
	} else if t.requestLimit > 0 {
		left := max(t.requestLimit-t.requests, 0)
		reqs = fmt.Sprintf("requests_left=%d/%d", left, t.requestLimit)
		low = low || left*5 <= t.requestLimit
	}
	if t.tokenBudget > 0 {
		left := max(t.tokenBudget-t.tokens, 0)
		toks = fmt.Sprintf("tokens_left=%d/%d", left, t.tokenBudget)
		low = low || left*5 <= t.tokenBudget
	}
	s := reqs + " " + toks
	if low {
		s = strings.TrimSpace(s) + " LOW"
	}
	return s
}

// UsageSnapshot is the day's Gemini usage as exposed on /stats. Left values
// are nil when the matching limit is not configured (Gemini reports none).
type UsageSnapshot struct {
	Model           string `json:"model"`
	Requests        int    `json:"requests"`
	Tokens          int    `json:"tokens"`
	RequestLimit    int    `json:"request_limit,omitempty"`
	RequestsLeft    *int   `json:"requests_left,omitempty"`
	TokenBudget     int    `json:"token_budget,omitempty"`
	TokensLeft      *int   `json:"tokens_left,omitempty"`
	Exhausted       bool   `json:"exhausted"`
	ResetsInSeconds int    `json:"resets_in_seconds"`
}

// UsageReporter is implemented by backends that track their own usage. The
// orchestrator type-asserts for it, so other backends need not care.
type UsageReporter interface {
	Usage() (UsageSnapshot, bool)
}

func (t *usageTracker) snapshot(model string) UsageSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	resetIn := t.rollover()
	s := UsageSnapshot{
		Model: model, Requests: t.requests, Tokens: t.tokens,
		RequestLimit: t.requestLimit, TokenBudget: t.tokenBudget,
		Exhausted: t.exhausted, ResetsInSeconds: int(resetIn.Seconds()),
	}
	if t.requestLimit > 0 {
		left := max(t.requestLimit-t.requests, 0)
		if t.exhausted {
			left = 0
		}
		s.RequestsLeft = &left
	}
	if t.tokenBudget > 0 {
		left := max(t.tokenBudget-t.tokens, 0)
		s.TokensLeft = &left
	}
	return s
}
