// Package opper is the hosted Opper Chat Completions llm.Provider.
//
// It is pure Go, available in every build. Opper is an EU-hosted LLM
// gateway that fronts many vendors behind one OpenAI-compatible
// /chat/completions endpoint under https://api.opper.ai/v3/compat;
// model ids are bare pool names, e.g. "gpt-5.4-mini" or
// "claude-sonnet-4-6", and a "provider/model" id pins one route. The
// wire format, the json_schema structured-output mechanism, and the
// hollow-200 retry all live in the shared openaicompat.Client; this
// package is just the constructor that addresses the gateway with a
// Bearer key.
package opper

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/llm"
	"github.com/zzet/gortex/internal/llm/provider/openaicompat"
)

// New constructs the Opper provider. The API key is read from the env
// var named by cfg.APIKeyEnv (default OPPER_API_KEY); an unset key is a
// hard error.
//
// Opper's base URL already carries its version segment
// ("https://api.opper.ai/v3/compat"), so "/chat/completions" is
// appended to it as is.
func New(cfg llm.RemoteConfig) (llm.Provider, error) {
	keyEnv := strings.TrimSpace(cfg.APIKeyEnv)
	if keyEnv == "" {
		keyEnv = "OPPER_API_KEY"
	}
	key := strings.TrimSpace(os.Getenv(keyEnv))
	if key == "" {
		return nil, fmt.Errorf("opper: API key env %q is not set", keyEnv)
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("opper: llm.opper.model is empty")
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = "https://api.opper.ai/v3/compat"
	}
	return &openaicompat.Client{
		ProviderID:      "opper",
		Tag:             "opper",
		Model:           cfg.Model,
		URL:             base + "/chat/completions",
		Headers:         map[string]string{"authorization": "Bearer " + key},
		HTTPClient:      &http.Client{Timeout: 120 * time.Second},
		SchemaMode:      openaicompat.SchemaJSONSchema,
		ReasoningEffort: strings.TrimSpace(cfg.Effort),
	}, nil
}
