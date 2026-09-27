package jev

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ByteDeskAI/bytedesk-sdk-dependencies/v2/aidecision"
)

func textValue(value string) aidecision.StructuredValue {
	return aidecision.StructuredValue{Format: aidecision.FormatText, Content: aidecision.TextInput{Inline: value}}
}
func mixedBatch() aidecision.BatchRequest {
	return aidecision.BatchRequest{ProviderID: "jev", Model: DefaultModel, Purpose: aidecision.PurposeEvaluation, IdempotencyKey: "fixture", State: textValue("Customer needs help"), Questions: []aidecision.Question{
		{ID: "choice", Kind: aidecision.KindChoice, Instructions: textValue("Which team?"), Choice: &aidecision.ChoiceQuestion{Options: []aidecision.Option{{ID: "billing", Description: nil}, {ID: "support", Description: pointerValue(textValue("Technical help"))}}}},
		{ID: "score", Kind: aidecision.KindScore, Instructions: textValue("How urgent?"), Score: &aidecision.ScoreQuestion{Levels: []aidecision.StructuredValue{textValue("Low"), textValue("High")}}},
		{ID: "noul", Kind: aidecision.KindNoul, Instructions: textValue("Is it urgent?"), Noul: &aidecision.NoulQuestion{}},
	}}
}
func pointerValue(v aidecision.StructuredValue) *aidecision.StructuredValue { return &v }

const validMixedResponse = `{"model":"jev-1.13.0","answers":{"noul":{"type":"noul","noul":9.5e-1},"score":{"type":"score","score":0.75,"legend":{"1":"High","0":"Low"},"probabilities":{"1":0.75,"0":0.25},"confidence":0.5},"choice":{"type":"choice","choice":"support","probabilities":{"support":0.8,"billing":0.2},"confidence":0.6}},"usage":{"input_tokens":300,"output_tokens":20}}`

func TestMixedRequestAndExactAnswerMapping(t *testing.T) {
	req := mixedBatch()
	body, err := EncodeRequest(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	var questions map[string]map[string]json.RawMessage
	_ = json.Unmarshal(wire["questions"], &questions)
	if len(questions) != 3 || string(questions["choice"]["criteria"]) != `{"billing":null,"support":"Technical help"}` {
		t.Fatalf("wrong criteria: %s", body)
	}
	result, err := DecodeResponse(req, []byte(validMixedResponse))
	if err != nil {
		t.Fatal(err)
	}
	if result.Answers[0].ID != "choice" || result.Answers[1].Score.Legend[0].ID != "0" || result.Answers[2].Noul.Noul != "0.95" {
		t.Fatalf("answer mapping: %+v", result)
	}
	if result.Usage.InputTokens == nil || *result.Usage.InputTokens != 300 {
		t.Fatal("usage lost")
	}
}

func TestResponseRejectsMalformedOrUncorrelatedAnswers(t *testing.T) {
	for _, raw := range []string{
		strings.Replace(validMixedResponse, `"noul":9.5e-1`, `"noul":"0.95"`, 1),
		strings.Replace(validMixedResponse, `"noul":9.5e-1`, `"noul":2`, 1),
		strings.Replace(validMixedResponse, `"input_tokens":300,`, "", 1),
		strings.Replace(validMixedResponse, `"support":0.8`, `"support":0.7`, 1),
		strings.Replace(validMixedResponse, `"choice":"support"`, `"choice":"invented"`, 1),
		strings.Replace(validMixedResponse, `"type":"noul"`, `"type":"score"`, 1),
		strings.Replace(validMixedResponse, `"model":"jev-1.13.0"`, `"model":"other"`, 1),
		strings.Replace(validMixedResponse, `"model":"jev-1.13.0"`, `"model":"wrong","model":"jev-1.13.0"`, 1),
		validMixedResponse + `{}`,
	} {
		if _, err := DecodeResponse(mixedBatch(), []byte(raw)); err == nil {
			t.Fatalf("invalid upstream response accepted: %s", raw)
		}
	}
}

func TestStructuredInputsRemainJSONAndPayloadResolutionIsBounded(t *testing.T) {
	req := mixedBatch()
	req.State = aidecision.StructuredValue{Format: aidecision.FormatJSON, Content: aidecision.TextInput{HandleID: "state"}}
	resolver := func(_ context.Context, id string) ([]byte, error) {
		if id != "state" {
			t.Fatal(id)
		}
		return []byte(`{"id":9007199254740993,"items":["a"]}`), nil
	}
	body, err := EncodeRequest(context.Background(), req, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"state":{"id":9007199254740993`) {
		t.Fatalf("structured value changed: %s", body)
	}
	if _, err := EncodeRequest(context.Background(), req, nil); err == nil {
		t.Fatal("unresolved handle accepted")
	}
	req.State = textValue(strings.Repeat("x", 32000))
	if _, err := EncodeRequest(context.Background(), req, nil); err == nil {
		t.Fatal("upstream state/question bound ignored")
	}
	req = mixedBatch()
	req.Questions[0].Choice.Options = append(req.Questions[0].Choice.Options, req.Questions[0].Choice.Options[0])
	if _, err := EncodeRequest(context.Background(), req, nil); err == nil {
		t.Fatal("duplicate choice key accepted")
	}
}

func TestExplicitAliasRetainsRequestedAndResolvedModel(t *testing.T) {
	req := mixedBatch()
	req.Model = "jev-latest"
	result, err := DecodeResponse(req, []byte(validMixedResponse))
	if err != nil {
		t.Fatal(err)
	}
	if result.RequestedModel != "jev-latest" || result.Model != DefaultModel {
		t.Fatalf("model identity lost: %+v", result)
	}
	for _, model := range []string{"jev-latest", "not-jev", "jev-1.13"} {
		body := strings.Replace(validMixedResponse, DefaultModel, model, 1)
		if _, err := DecodeResponse(req, []byte(body)); err == nil {
			t.Fatalf("unresolved alias accepted: %s", model)
		}
	}
}

func TestStructuredJSONRejectsDuplicatesDepthAndCancellation(t *testing.T) {
	for _, raw := range []string{`{"a":1,"\u0061":2}`, strings.Repeat("[", 66) + "0" + strings.Repeat("]", 66), `{} []`} {
		if uniqueJSON([]byte(raw)) == nil {
			t.Fatal("ambiguous JSON accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := EncodeRequest(ctx, mixedBatch(), nil); err == nil {
		t.Fatal("cancelled batch encoded")
	}
}
