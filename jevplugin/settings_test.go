package jev

import (
	"strings"
	"testing"

	"github.com/ByteDeskAI/bytedesk-sdk-dependencies/v2/hostsettings"
)

func TestSettingsRedactionAndExplicitAliases(t *testing.T) {
	for _, values := range []string{`{}`, `{"model":"jev-1.13.0"}`, `{"model":"jev-latest","allowAliases":true}`, `{"requestsPerMinute":null,"dailyInputTokens":null}`} {
		result := ValidateSettings(hostsettings.ValidateRequest{SectionID: "jev", ValuesJSON: values})
		if !result.Valid {
			t.Fatalf("valid settings refused: %+v", result)
		}
	}
	for _, values := range []string{`{"apiKey":"must-not-leak"}`, `{"model":"jev-latest"}`, `{"model":"not-approved"}`, `{"requestsPerMinute":-1}`, `{"dailyInputTokens":0}`, `{"unknown":true}`, `{"model":"jev-1.13.0","model":"jev-latest"}`} {
		result := ValidateSettings(hostsettings.ValidateRequest{SectionID: "jev", ValuesJSON: values})
		if result.Valid {
			t.Fatalf("unsafe settings accepted: %s", values)
		}
		for _, field := range result.Errors {
			if strings.Contains(field.Message, "must-not-leak") {
				t.Fatal("secret disclosed")
			}
		}
	}
}
