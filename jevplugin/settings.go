package jev

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ByteDeskAI/bytedesk-sdk-dependencies/v2/hostsettings"
	"github.com/ByteDeskAI/bytedesk-sdk-dependencies/v2/plugin"
)

// Settings deliberately has no API-key value. Only the host owns that slot.
type Settings struct {
	Model             string `json:"model"`
	AllowAliases      bool   `json:"allowAliases"`
	RequestsPerMinute *int   `json:"requestsPerMinute"`
	DailyInputTokens  *int   `json:"dailyInputTokens"`
}

func settingsFields() []plugin.ConfigField {
	one := 1
	return []plugin.ConfigField{
		{Key: "apiKey", Kind: plugin.ConfigKindSecret, Label: "Typesafe API key", Description: "Write-only. Gateway stores and uses this key; Jev receives only a scoped handle."},
		{Key: "model", Kind: plugin.ConfigKindEnum, Label: "Jev model", Default: DefaultModel, Choices: []string{DefaultModel, "jev-latest", "jev-preview"}, Description: "Pin a version for repeatable decisions. Aliases also require Allow model aliases."},
		{Key: "allowAliases", Kind: plugin.ConfigKindBool, Label: "Allow model aliases", Default: "false", Description: "An alias can resolve to a different model after an upstream release."},
		{Key: "requestsPerMinute", Kind: plugin.ConfigKindInt, Label: "Requests per minute per consumer", Nullable: true, Min: &one, Description: "Optional host admission limit, applied separately to each consumer plugin, not an aggregate Jev total. Unset means no additional local limit."},
		{Key: "dailyInputTokens", Kind: plugin.ConfigKindInt, Label: "Daily input tokens per consumer", Nullable: true, Min: &one, Description: "Optional host-accounted limit, applied separately to each consumer plugin. Unknown usage never counts as zero; bounded in-flight overshoot is possible."},
	}
}

func ParseSettings(values string) (Settings, error) {
	settings := Settings{Model: DefaultModel}
	raw := bytes.TrimSpace([]byte(values))
	if len(raw) == 0 || len(values) > 32768 || raw[0] != '{' || uniqueJSON(raw) != nil {
		return settings, errors.New("invalid settings JSON object")
	}
	decoder := json.NewDecoder(bytes.NewBufferString(values))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&settings); err != nil {
		return settings, errors.New("settings contain unsupported or secret fields")
	}
	if settings.Model == "" {
		return settings, errors.New("model is required")
	}
	if settings.Model != DefaultModel && !(settings.AllowAliases && (settings.Model == "jev-latest" || settings.Model == "jev-preview")) {
		return settings, errors.New("model must be pinned or an explicitly allowed Jev alias")
	}
	if settings.RequestsPerMinute != nil && *settings.RequestsPerMinute < 1 {
		return settings, errors.New("requestsPerMinute must be positive or unset")
	}
	if settings.DailyInputTokens != nil && *settings.DailyInputTokens < 1 {
		return settings, errors.New("dailyInputTokens must be positive or unset")
	}
	return settings, nil
}

func ValidateSettings(request hostsettings.ValidateRequest) hostsettings.ValidateResult {
	var err error
	if request.SectionID != "jev" {
		err = errors.New("settings section does not belong to Jev")
	} else if check := request.Validate(); check != nil {
		err = errors.New("invalid settings validation request")
	} else {
		_, err = ParseSettings(request.ValuesJSON)
	}
	if err != nil {
		return hostsettings.ValidateResult{Valid: false, Errors: []hostsettings.FieldError{{Field: "model", Message: fmt.Sprint(err)}}}
	}
	return hostsettings.ValidateResult{Valid: true, Errors: []hostsettings.FieldError{}}
}
