// Package provider - decision service.
// Ported from astrbot/core/provider/decision.py (4.29.4)
package provider

import (
	"context"
	"fmt"
	"strings"
)

// DecisionProviderLookup resolves decision providers by id or default.
type DecisionProviderLookup interface {
	// GetDecisionProvider returns the default decision provider (nil if none).
	GetDecisionProvider() DecisionProvider
	// Get returns a provider by id (nil if absent).
	Get(id string) AbstractProvider
}

// DecisionService exposes decision questions without coupling callers to a
// provider adapter (aligned with py DecisionService).
type DecisionService struct {
	manager DecisionProviderLookup
}

// NewDecisionService creates a decision service.
func NewDecisionService(manager DecisionProviderLookup) *DecisionService {
	return &DecisionService{manager: manager}
}

func (s *DecisionService) resolve(providerID string) (DecisionProvider, error) {
	if s.manager == nil {
		return nil, NewDecisionError("Decision provider is not configured")
	}
	if providerID != "" {
		if p, ok := s.manager.Get(providerID).(DecisionProvider); ok && p != nil {
			return p, nil
		}
		return nil, NewDecisionError("Decision provider %s is not configured", providerID)
	}
	p := s.manager.GetDecisionProvider()
	if p == nil {
		return nil, NewDecisionError("Decision provider is not configured")
	}
	return p, nil
}

// Ask answers multiple decision questions in one request.
func (s *DecisionService) Ask(ctx context.Context, questions map[string]DecisionQuestion, state DecisionState, providerID string) (map[string]DecisionAnswer, error) {
	provider, err := s.resolve(providerID)
	if err != nil {
		return nil, err
	}
	return provider.Ask(ctx, questions, state, 0)
}

// Choose selects one option for a question. criteria may be a map of
// option→description or an ordered slice of option values.
func (s *DecisionService) Choose(ctx context.Context, question string, criteria interface{}, state DecisionState, providerID string) (*DecisionChoiceAnswer, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return nil, fmt.Errorf("question must not be empty")
	}
	values, descriptions, err := normalizeCriteria(criteria)
	if err != nil {
		return nil, err
	}
	if len(values) < 2 {
		return nil, fmt.Errorf("criteria must contain at least two values")
	}
	seen := map[string]struct{}{}
	for _, v := range values {
		if _, ok := seen[v]; ok {
			return nil, fmt.Errorf("criteria must be unique")
		}
		seen[v] = struct{}{}
	}

	optionIDs := map[string]string{}
	payload := map[string]string{}
	for i, value := range values {
		id := fmt.Sprintf("o%d", i+1)
		optionIDs[id] = value
		payload[id] = descriptions[i]
	}

	answers, err := s.Ask(ctx, map[string]DecisionQuestion{
		"answer": DecisionChoiceQuestion{Instructions: question, Criteria: payload},
	}, state, providerID)
	if err != nil {
		return nil, err
	}
	answer, ok := answers["answer"].(DecisionChoiceAnswer)
	if !ok {
		return nil, NewDecisionError("Decision provider returned no choice answer")
	}
	chosen, ok := optionIDs[answer.Choice]
	if !ok || len(answer.Probabilities) != len(optionIDs) {
		return nil, NewDecisionError("Decision provider returned an invalid choice")
	}
	remapped := make(map[string]float64, len(answer.Probabilities))
	for optionID, probability := range answer.Probabilities {
		value, ok := optionIDs[optionID]
		if !ok {
			return nil, NewDecisionError("Decision provider returned an invalid choice")
		}
		remapped[value] = probability
	}
	return &DecisionChoiceAnswer{
		Choice:        chosen,
		Probability:   answer.Probability,
		Probabilities: remapped,
		Confidence:    answer.Confidence,
	}, nil
}

// YesNo returns whether a statement is true at the given probability
// threshold. When defaultVal is non-nil it is returned on provider failure.
func (s *DecisionService) YesNo(ctx context.Context, question string, state DecisionState, threshold float64, providerID string, defaultVal *bool) (bool, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return false, fmt.Errorf("question must not be empty")
	}
	if threshold < 0 || threshold > 1 {
		return false, fmt.Errorf("threshold must be between 0 and 1")
	}
	answers, err := s.Ask(ctx, map[string]DecisionQuestion{
		"answer": DecisionBooleanQuestion{Instructions: question},
	}, state, providerID)
	if err != nil {
		if defaultVal == nil {
			return false, err
		}
		logger.Warn("[Decision] yes_no failed; using default %v: %v", *defaultVal, err)
		return *defaultVal, nil
	}
	answer, ok := answers["answer"].(DecisionBooleanAnswer)
	if !ok {
		err := NewDecisionError("Decision provider returned no boolean answer")
		if defaultVal == nil {
			return false, err
		}
		logger.Warn("[Decision] yes_no failed; using default %v: %v", *defaultVal, err)
		return *defaultVal, nil
	}
	return answer.Probability >= threshold, nil
}

// normalizeCriteria returns the option values and their descriptions.
func normalizeCriteria(criteria interface{}) ([]string, []string, error) {
	switch c := criteria.(type) {
	case map[string]string:
		values := make([]string, 0, len(c))
		descriptions := make([]string, 0, len(c))
		for value, description := range c {
			values = append(values, value)
			descriptions = append(descriptions, description)
		}
		return values, descriptions, nil
	case map[string]interface{}:
		values := make([]string, 0, len(c))
		descriptions := make([]string, 0, len(c))
		for value, description := range c {
			values = append(values, value)
			descriptions = append(descriptions, fmt.Sprintf("%v", description))
		}
		return values, descriptions, nil
	case []string:
		values := make([]string, len(c))
		descriptions := make([]string, len(c))
		for i, value := range c {
			values[i] = value
			descriptions[i] = value
		}
		return values, descriptions, nil
	case []interface{}:
		values := make([]string, len(c))
		descriptions := make([]string, len(c))
		for i, value := range c {
			s := fmt.Sprintf("%v", value)
			values[i] = s
			descriptions[i] = s
		}
		return values, descriptions, nil
	default:
		return nil, nil, fmt.Errorf("criteria must be a map or a slice of values")
	}
}
