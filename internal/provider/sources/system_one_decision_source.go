// Package sources - System One / Jev decision provider.
// Ported from astrbot/core/provider/sources/system_one_decision_source.py
package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
)

const (
	systemOneDefaultTotalTimeout     = 5.0
	systemOneDefaultMaxConcurrency   = 4
	systemOneResponseSummaryChars    = 500
	systemOneDefaultModel            = "jev-latest"
	systemOneDecisionProviderAdapter = "system_one_decision"
)

// SystemOneDecisionSource is a System One compatible provider for structured
// decisions.
type SystemOneDecisionSource struct {
	*provider.BaseProvider
	baseURL      string
	model        string
	apiKey       string
	proxy        string
	timeout      time.Duration
	totalTimeout time.Duration
	sem          chan struct{}
	client       *http.Client
}

// NewSystemOneDecisionSource creates a System One decision provider.
func NewSystemOneDecisionSource(config, settings map[string]interface{}) (*SystemOneDecisionSource, error) {
	bp := provider.NewBaseProvider(config, settings)

	rawBase := strings.TrimSpace(configString(config, "api_base", ""))
	exactEndpoint := strings.HasSuffix(rawBase, "/")
	baseURL := strings.TrimRight(rawBase, "/")
	if baseURL == "" {
		return nil, fmt.Errorf("System One decision provider requires api_base")
	}
	if !exactEndpoint && !strings.HasSuffix(baseURL, "/systemone") {
		baseURL = baseURL + "/systemone"
	}

	apiKey, _ := config["key"].(string)
	if _, isStr := config["key"].(string); config["key"] != nil && !isStr {
		return nil, fmt.Errorf("System One API key must be a string")
	}

	timeout := time.Duration(configFloat(config, "timeout", 2) * float64(time.Second))
	totalTimeout := configFloat(config, "total_timeout", systemOneDefaultTotalTimeout)
	if totalTimeout <= 0 {
		return nil, fmt.Errorf("total_timeout must be greater than 0")
	}
	maxConcurrency := configInt(config, "max_concurrency", systemOneDefaultMaxConcurrency)
	if maxConcurrency <= 0 {
		return nil, fmt.Errorf("max_concurrency must be greater than 0")
	}

	proxy := strings.TrimSpace(configString(config, "proxy", ""))
	transport := &http.Transport{}
	if proxy != "" {
		if u, err := url.Parse(proxy); err == nil {
			transport.Proxy = http.ProxyURL(u)
		}
	}

	model := configString(config, "model", systemOneDefaultModel)
	s := &SystemOneDecisionSource{
		BaseProvider: bp,
		baseURL:      baseURL,
		model:        model,
		apiKey:       apiKey,
		proxy:        proxy,
		timeout:      timeout,
		totalTimeout: time.Duration(totalTimeout * float64(time.Second)),
		sem:          make(chan struct{}, maxConcurrency),
		client:       &http.Client{Timeout: timeout, Transport: transport},
	}
	s.SetModel(model)
	s.SetCapability(provider.CapDecision)
	return s, nil
}

// isClose reports whether a and b are within a small epsilon (used for the
// decision contract's probability-sum and argmax checks).
func isClose(a, b float64) bool {
	return math.Abs(a-b) <= 1e-9
}

// summarizeResponse renders a bounded, human-readable summary of a response.
func summarizeResponse(value interface{}) string {
	raw, err := json.Marshal(value)
	summary := string(raw)
	if err != nil {
		summary = fmt.Sprintf("%v", value)
	}
	if len(summary) <= systemOneResponseSummaryChars {
		return summary
	}
	return summary[:systemOneResponseSummaryChars-3] + "..."
}

func isFiniteNumber(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		if math.IsInf(n, 0) || math.IsNaN(n) {
			return 0, false
		}
		return n, true
	case float32:
		f := float64(n)
		if math.IsInf(f, 0) || math.IsNaN(f) {
			return 0, false
		}
		return f, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// Ask answers multiple questions through one System One request.
func (s *SystemOneDecisionSource) Ask(ctx context.Context, questions map[string]provider.DecisionQuestion, state provider.DecisionState, requestMaxRetries int) (map[string]provider.DecisionAnswer, error) {
	if len(questions) == 0 {
		return nil, fmt.Errorf("questions must not be empty")
	}

	payloadQuestions := map[string]interface{}{}
	for head, question := range questions {
		var instructions string
		switch q := question.(type) {
		case provider.DecisionChoiceQuestion:
			instructions = strings.TrimSpace(q.Instructions)
			if instructions == "" {
				return nil, fmt.Errorf("question %s must not be empty", head)
			}
			if len(q.Criteria) < 2 {
				return nil, fmt.Errorf("question %s requires at least two criteria entries", head)
			}
			payloadQuestions[head] = map[string]interface{}{
				"type":         "choice",
				"instructions": instructions,
				"criteria":     q.Criteria,
			}
		case provider.DecisionScoreQuestion:
			instructions = strings.TrimSpace(q.Instructions)
			if instructions == "" {
				return nil, fmt.Errorf("question %s must not be empty", head)
			}
			valid := len(q.Criteria) >= 2 && len(q.Criteria) <= 10
			if valid {
				for _, level := range q.Criteria {
					if strings.TrimSpace(level) == "" {
						valid = false
						break
					}
				}
			}
			if !valid {
				return nil, fmt.Errorf("question %s requires 2 to 10 non-empty criteria entries", head)
			}
			payloadQuestions[head] = map[string]interface{}{
				"type":         "score",
				"instructions": instructions,
				"criteria":     q.Criteria,
			}
		case provider.DecisionBooleanQuestion:
			instructions = strings.TrimSpace(q.Instructions)
			if instructions == "" {
				return nil, fmt.Errorf("question %s must not be empty", head)
			}
			payloadQuestions[head] = map[string]interface{}{
				"type":         "noul",
				"instructions": instructions,
			}
		default:
			return nil, fmt.Errorf("question %s has an unsupported type", head)
		}
	}

	body, err := json.Marshal(map[string]interface{}{
		"model":     s.model,
		"state":     stateOrEmpty(state),
		"questions": payloadQuestions,
	})
	if err != nil {
		return nil, provider.NewDecisionError("System One request could not be encoded: %v", err)
	}

	requestCtx, cancel := context.WithTimeout(ctx, s.totalTimeout)
	defer cancel()

	cfg := DefaultRetryConfig()
	if requestMaxRetries > 0 {
		cfg.MaxAttempts = requestMaxRetries + 1
	} else {
		cfg.MaxAttempts = 1
	}
	if cfg.Retry429Enabled == nil {
		on := true
		cfg.Retry429Enabled = &on
	}

	// 并发闸门 + 重试（对齐 py asyncio.Semaphore + retry_provider_request）。
	select {
	case s.sem <- struct{}{}:
	case <-requestCtx.Done():
		return nil, provider.NewDecisionError("System One total request timed out after %gs", s.totalTimeout.Seconds())
	}
	defer func() { <-s.sem }()

	resp, err := DoWithRetry(requestCtx, s.client, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, s.baseURL, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		for k, v := range s.RequestHeaders() {
			req.Header.Set(k, v)
		}
		if s.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+s.apiKey)
		}
		return req, nil
	}, cfg, "System One Decision")
	if err != nil {
		if requestCtx.Err() == context.DeadlineExceeded {
			return nil, provider.NewDecisionError("System One total request timed out after %gs", s.totalTimeout.Seconds())
		}
		if strings.Contains(strings.ToLower(err.Error()), "timeout") {
			return nil, provider.NewDecisionError("System One attempt timed out after %gs", s.timeout.Seconds())
		}
		return nil, provider.NewDecisionError("System One request failed: unable to connect (check network or proxy)")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, provider.NewDecisionError("System One endpoint returned HTTP %d", resp.StatusCode)
	}

	var decoded map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, provider.NewDecisionError("System One endpoint returned invalid JSON or text encoding")
	}
	rawAnswers, ok := decoded["answers"].(map[string]interface{})
	if !ok {
		summary := summarizeResponse(decoded)
		logger.Warn("[System One Decision] Invalid response: %s", summary)
		return nil, provider.NewDecisionError("System One endpoint returned an invalid response: %s", summary)
	}

	answers := map[string]provider.DecisionAnswer{}
	for head, question := range questions {
		rawAnswer, ok := rawAnswers[head].(map[string]interface{})
		if !ok {
			summary := summarizeResponse(rawAnswers)
			logger.Warn("[System One Decision] Missing answer for %s: %s", head, summary)
			return nil, provider.NewDecisionError("System One endpoint returned no answer for %s: %s", head, summary)
		}
		switch q := question.(type) {
		case provider.DecisionScoreQuestion:
			answer, err := validateScoreAnswer(head, q, rawAnswer)
			if err != nil {
				return nil, err
			}
			answers[head] = answer
		case provider.DecisionChoiceQuestion:
			answer, err := validateChoiceAnswer(head, q, rawAnswer)
			if err != nil {
				return nil, err
			}
			answers[head] = answer
		default:
			probability, ok := isFiniteNumber(rawAnswer["noul"])
			if !ok || probability < 0 || probability > 1 {
				summary := summarizeResponse(rawAnswer)
				logger.Warn("[System One Decision] Invalid boolean answer for %s: %s", head, summary)
				return nil, provider.NewDecisionError("System One endpoint returned an invalid boolean answer for %s: %s", head, summary)
			}
			answers[head] = provider.DecisionBooleanAnswer{Probability: probability}
		}
	}
	return answers, nil
}

func stateOrEmpty(state provider.DecisionState) interface{} {
	if state == nil {
		return map[string]interface{}{}
	}
	return state
}

func validateScoreAnswer(head string, q provider.DecisionScoreQuestion, raw map[string]interface{}) (provider.DecisionAnswer, error) {
	score, scoreOK := isFiniteNumber(raw["score"])
	confidence, confOK := isFiniteNumber(raw["confidence"])
	probabilities, probOK := raw["probabilities"].(map[string]interface{})
	legend, legendOK := raw["legend"].(map[string]interface{})

	expectedLegend := map[string]string{}
	for index, level := range q.Criteria {
		expectedLegend[fmt.Sprintf("%d", index)] = level
	}

	validScore := scoreOK && score >= 0 && score <= float64(len(q.Criteria)-1)
	validProbabilities := probOK && confOK && confidence >= 0 && confidence <= 1
	if validProbabilities {
		sum := 0.0
		for _, v := range probabilities {
			f, ok := isFiniteNumber(v)
			if !ok || f < 0 || f > 1 {
				validProbabilities = false
				break
			}
			sum += f
		}
		if validProbabilities && !isClose(sum, 1.0) {
			validProbabilities = false
		}
	}
	legendMatches := false
	if legendOK && len(legend) == len(expectedLegend) {
		legendMatches = true
		for k, v := range expectedLegend {
			if s, ok := legend[k].(string); !ok || s != v {
				legendMatches = false
				break
			}
		}
	}
	probKeysMatch := false
	if probOK && len(probabilities) == len(expectedLegend) {
		probKeysMatch = true
		for k := range expectedLegend {
			if _, ok := probabilities[k]; !ok {
				probKeysMatch = false
				break
			}
		}
	}
	if !(validScore && validProbabilities && legendMatches && probKeysMatch) {
		summary := summarizeResponse(raw)
		logger.Warn("[System One Decision] Invalid score answer for %s: %s", head, summary)
		return nil, provider.NewDecisionError("System One endpoint returned an invalid score answer for %s: %s", head, summary)
	}
	probs := map[string]float64{}
	for k, v := range probabilities {
		f, _ := isFiniteNumber(v)
		probs[k] = f
	}
	return provider.DecisionScoreAnswer{
		Score:         score,
		Legend:        expectedLegend,
		Probabilities: probs,
		Confidence:    confidence,
	}, nil
}

func validateChoiceAnswer(head string, q provider.DecisionChoiceQuestion, raw map[string]interface{}) (provider.DecisionAnswer, error) {
	choice, choiceOK := raw["choice"].(string)
	confidence, confOK := isFiniteNumber(raw["confidence"])
	probabilities, probOK := raw["probabilities"].(map[string]interface{})

	validNumbers := confOK && confidence >= 0 && confidence <= 1
	maxProb := -1.0
	if probOK {
		for _, v := range probabilities {
			f, ok := isFiniteNumber(v)
			if !ok || f < 0 || f > 1 {
				validNumbers = false
				break
			}
			if f > maxProb {
				maxProb = f
			}
		}
	} else {
		validNumbers = false
	}
	criteriaMatch := false
	if probOK && len(probabilities) == len(q.Criteria) {
		criteriaMatch = true
		for k := range q.Criteria {
			if _, ok := probabilities[k]; !ok {
				criteriaMatch = false
				break
			}
		}
	}
	choiceInCriteria := false
	if choiceOK {
		_, choiceInCriteria = q.Criteria[choice]
	}
	sum := 0.0
	if probOK {
		for _, v := range probabilities {
			f, _ := isFiniteNumber(v)
			sum += f
		}
	}
	choiceIsMax := false
	if choiceOK && probOK && maxProb >= 0 {
		if f, ok := isFiniteNumber(probabilities[choice]); ok {
			choiceIsMax = isClose(f, maxProb)
		}
	}
	if !(validNumbers && criteriaMatch && choiceInCriteria && isClose(sum, 1.0) && choiceIsMax) {
		summary := summarizeResponse(raw)
		logger.Warn("[System One Decision] Invalid choice answer for %s: %s", head, summary)
		return nil, provider.NewDecisionError("System One endpoint returned an invalid choice answer for %s: %s", head, summary)
	}
	probs := map[string]float64{}
	for k, v := range probabilities {
		f, _ := isFiniteNumber(v)
		probs[k] = f
	}
	selected, _ := isFiniteNumber(probabilities[choice])
	return provider.DecisionChoiceAnswer{
		Choice:        choice,
		Probability:   selected,
		Probabilities: probs,
		Confidence:    confidence,
	}, nil
}

// Test verifies the provider by asking a trivial boolean question.
func (s *SystemOneDecisionSource) Test(ctx context.Context) error {
	_, err := s.Ask(ctx, map[string]provider.DecisionQuestion{
		"test": provider.DecisionBooleanQuestion{Instructions: "Is this provider available?"},
	}, nil, 0)
	return err
}
