package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"

	"github.com/ByteDeskAI/bytedesk-sdk-dependencies/v2/aidecision"
	"github.com/ByteDeskAI/bytedesk-sdk-dependencies/v2/payloads"
)

const DefaultModel = "jev-1.13.0"

var versionedModel = regexp.MustCompile(`^jev-[0-9]+\.[0-9]+\.[0-9]+$`)

// Jev's documented token limits are enforced independently of the host's
// 8 MiB transport ceiling. Without an authoritative tokenizer we deliberately
// use conservative UTF-8/JSON byte guards, reserving space for framing. These
// are admission bounds, never billing-token estimates. Upstream remains the
// authority on its exact tokenizer and can still reject a request with 422.
const maxRequestBytes = 64000 - 1024
const maxStateQuestionBytes = 32000 - 512

type PayloadResolver func(context.Context, string) ([]byte, error)
type upstreamQuestion struct {
	Type         string          `json:"type"`
	Instructions json.RawMessage `json:"instructions"`
	Criteria     any             `json:"criteria,omitempty"`
}
type upstreamRequest struct {
	State     json.RawMessage             `json:"state"`
	Model     string                      `json:"model"`
	Questions map[string]upstreamQuestion `json:"questions"`
}

// EncodeRequest translates SDK primitives to the Typesafe endpoint's JSON. It
// knows no URL, HTTP header, credential, process or filesystem location.
func EncodeRequest(ctx context.Context, request aidecision.BatchRequest, resolve PayloadResolver) ([]byte, error) {
	if err := request.Validate(); err != nil {
		return nil, fmt.Errorf("invalid decision batch: %w", err)
	}
	cache := map[string][]byte{}
	resolvedBytes := 0
	value := func(v aidecision.StructuredValue) (json.RawMessage, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		content := []byte(v.Content.Inline)
		if id := v.Content.HandleID; id != "" {
			if resolve == nil {
				return nil, errors.New("scoped payload resolver required")
			}
			var ok bool
			content, ok = cache[id]
			if !ok {
				var err error
				content, err = resolve(ctx, id)
				if err != nil {
					return nil, err
				}
				if len(content) > maxRequestBytes-resolvedBytes {
					return nil, errors.New("resolved payloads exceed model input bound")
				}
				resolvedBytes += len(content)
				cache[id] = content
			}
		}
		if err := v.ValidateContent(content); err != nil {
			return nil, err
		}
		if v.Format == aidecision.FormatText {
			raw, err := json.Marshal(string(content))
			return raw, err
		}
		if err := uniqueJSON(content); err != nil {
			return nil, errors.New("structured JSON is ambiguous")
		}
		return json.RawMessage(content), nil
	}
	state, err := value(request.State)
	if err != nil {
		return nil, err
	}
	wire := upstreamRequest{State: state, Model: request.Model, Questions: map[string]upstreamQuestion{}}
	for _, q := range request.Questions {
		instructions, err := value(q.Instructions)
		if err != nil {
			return nil, err
		}
		question := upstreamQuestion{Type: q.Kind, Instructions: instructions}
		switch q.Kind {
		case aidecision.KindChoice:
			criteria := map[string]json.RawMessage{}
			for _, option := range q.Choice.Options {
				if option.Description == nil {
					criteria[option.ID] = json.RawMessage("null")
					continue
				}
				raw, err := value(*option.Description)
				if err != nil {
					return nil, err
				}
				criteria[option.ID] = raw
			}
			question.Criteria = criteria
		case aidecision.KindScore:
			criteria := make([]json.RawMessage, 0, len(q.Score.Levels))
			for _, level := range q.Score.Levels {
				raw, err := value(level)
				if err != nil {
					return nil, err
				}
				criteria = append(criteria, raw)
			}
			question.Criteria = criteria
		case aidecision.KindNoul:
			if q.Noul.Criteria != nil {
				yes, err := value(q.Noul.Criteria.True)
				if err != nil {
					return nil, err
				}
				no, err := value(q.Noul.Criteria.False)
				if err != nil {
					return nil, err
				}
				question.Criteria = map[string]json.RawMessage{"true": yes, "false": no}
			}
		}
		raw, err := json.Marshal(question)
		if err != nil {
			return nil, err
		}
		if len(state)+len(raw) > maxStateQuestionBytes {
			return nil, errors.New("Jev state plus longest question exceeds conservative model input bound")
		}
		wire.Questions[q.ID] = question
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}
	if len(body) > maxRequestBytes {
		return nil, errors.New("Jev batch exceeds conservative model input bound")
	}
	return body, nil
}

type upstreamAnswer struct {
	Type          string                     `json:"type"`
	Choice        string                     `json:"choice"`
	Noul          json.RawMessage            `json:"noul"`
	Score         json.RawMessage            `json:"score"`
	Confidence    json.RawMessage            `json:"confidence"`
	Probabilities map[string]json.RawMessage `json:"probabilities"`
	Legend        map[string]string          `json:"legend"`
}
type upstreamResponse struct {
	Model   string                    `json:"model"`
	Answers map[string]upstreamAnswer `json:"answers"`
	Usage   struct {
		InputTokens  *uint64 `json:"input_tokens"`
		OutputTokens *uint64 `json:"output_tokens"`
	} `json:"usage"`
}

func decimal(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || raw[0] == '"' {
		return "", errors.New("upstream numeric field missing or not a number")
	}
	return aidecision.DecimalFromJSONNumber(json.Number(raw))
}

// DecodeResponse refuses incomplete, ambiguous or unrelated results. Decimals
// never pass through float64, and output follows the request's question order.
func DecodeResponse(request aidecision.BatchRequest, body []byte) (aidecision.BatchResult, error) {
	var result aidecision.BatchResult
	if len(body) == 0 || len(body) > payloads.MaxAssembledBytes {
		return result, errors.New("upstream response exceeds bounds")
	}
	if err := uniqueJSON(body); err != nil {
		return result, errors.New("upstream response is invalid or ambiguous JSON")
	}
	var response upstreamResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return result, errors.New("upstream response has invalid field types")
	}
	if len(response.Answers) != len(request.Questions) {
		return result, errors.New("upstream answer count does not match questions")
	}
	if request.Model == "jev-latest" || request.Model == "jev-preview" {
		if !versionedModel.MatchString(response.Model) {
			return result, errors.New("Jev alias did not resolve to a versioned Jev model")
		}
	} else if response.Model != request.Model {
		return result, errors.New("upstream model does not match the pinned request")
	}
	result = aidecision.BatchResult{ProviderID: request.ProviderID, RequestedModel: request.Model, Model: response.Model, Usage: aidecision.Usage{InputTokens: response.Usage.InputTokens, OutputTokens: response.Usage.OutputTokens}}
	for _, q := range request.Questions {
		a, ok := response.Answers[q.ID]
		if !ok || a.Type != q.Kind {
			return result, errors.New("upstream answer does not match question")
		}
		answer := aidecision.Answer{ID: q.ID, Kind: q.Kind}
		if q.Kind == aidecision.KindNoul {
			n, err := decimal(a.Noul)
			if err != nil {
				return result, err
			}
			answer.Noul = &aidecision.NoulAnswer{Noul: n}
		} else {
			confidence, err := decimal(a.Confidence)
			if err != nil {
				return result, err
			}
			ids := make([]string, 0, len(a.Probabilities))
			for id := range a.Probabilities {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			probabilities := make([]aidecision.Probability, 0, len(ids))
			for _, id := range ids {
				n, err := decimal(a.Probabilities[id])
				if err != nil {
					return result, err
				}
				probabilities = append(probabilities, aidecision.Probability{ID: id, Value: n})
			}
			if q.Kind == aidecision.KindChoice {
				answer.Choice = &aidecision.ChoiceAnswer{Choice: a.Choice, Confidence: confidence, Probabilities: probabilities}
			} else {
				score, err := decimal(a.Score)
				if err != nil {
					return result, err
				}
				legend := make([]aidecision.LegendEntry, 0, len(a.Legend))
				for i := range len(a.Legend) {
					id := strconv.Itoa(i)
					label, ok := a.Legend[id]
					if !ok {
						return result, errors.New("upstream score legend must use contiguous zero-based indices")
					}
					legend = append(legend, aidecision.LegendEntry{ID: id, Label: label})
				}
				answer.Score = &aidecision.ScoreAnswer{Score: score, Confidence: confidence, Legend: legend, Probabilities: probabilities}
			}
		}
		result.Answers = append(result.Answers, answer)
	}
	if err := result.ValidateFor(request); err != nil {
		return result, fmt.Errorf("invalid upstream result: %w", err)
	}
	return result, nil
}

// uniqueJSON rejects duplicate keys (including escaped aliases), trailing
// values and excessive nesting, rather than letting last-key-wins alter input.
func uniqueJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var scan func(int) error
	scan = func(depth int) error {
		if depth > 64 {
			return errors.New("JSON nesting too deep")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			keys := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || keys[name] {
					return errors.New("duplicate JSON key")
				}
				keys[name] = true
				if err := scan(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := scan(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("unexpected JSON delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	if err := scan(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}
