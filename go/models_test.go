package main

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const testListing = `{
  "function_configs": [
    {
      "function": "chat_v3",
      "config_info_list": [
        {
          "config_name": "glm-5.2",
          "display_config": {"display_name": "GLM-5.2"},
          "context_window_tokens": {"dev": 200000, "max": 1000000},
          "model_detail_list": [
            {"model_name": "glm-5.2__dev", "max_tokens": 32000},
            {"model_name": "glm-5.2__max", "max_tokens": 64000}
          ]
        },
        {"config_name": "custom_model_1", "display_config": {"display_name": "Mine"}},
        {"config_name": "title-helper", "usage": "title_generation"}
      ]
    },
    {
      "function": "solo_agent",
      "config_info_list": [
        {
          "config_name": "deepseek-v4.1-flash",
          "display_config": {"display_name": "DeepSeek V4.1 Flash"},
          "context_window_tokens": {"dev": 128000},
          "model_detail_list": [{"model_name": "deepseek-v4.1-flash__dev", "max_tokens": 16000}],
          "reasoning_effort_config": {"options": ["light", "high", "extra_high"]},
          "multimodal": false
        }
      ]
    }
  ]
}`

func TestModelsForAuthReadsLiveList(t *testing.T) {
	c := testCredential(siteTraeCN)
	c.UID = "models-live"
	sender := &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if !strings.HasSuffix(req.URL, "/api/ide/v1/batch_get_detail_param") {
			t.Fatalf("unexpected request %s", req.URL)
		}
		return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(testListing)}, nil
	}}
	resp, err := modelsForAuth(pluginapi.AuthModelRequest{StorageJSON: credentialJSON(c)}, sender)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]pluginapi.ModelInfo{}
	for _, m := range resp.Models {
		byID[m.ID] = m
	}
	glm, ok := byID["glm-5.2"]
	if !ok {
		t.Fatalf("models = %#v", resp.Models)
	}
	if glm.ContextLength != 200000 || glm.OutputTokenLimit != 32000 || glm.Thinking != nil {
		t.Fatalf("glm = %#v", glm)
	}
	max, ok := byID["glm-5.2-max"]
	if !ok || max.ContextLength != 1000000 {
		t.Fatalf("max = %#v", max)
	}
	deep, ok := byID["deepseek-v4.1-flash"]
	if !ok {
		t.Fatalf("deepseek missing: %#v", resp.Models)
	}
	if deep.Thinking == nil || strings.Join(deep.Thinking.Levels, ",") != "low,high,xhigh" {
		t.Fatalf("deepseek = %#v", deep)
	}
	if len(deep.SupportedInputModalities) != 1 || deep.SupportedInputModalities[0] != "text" {
		t.Fatalf("deepseek modalities = %#v", deep.SupportedInputModalities)
	}
	if _, ok := byID["custom_model_1"]; ok {
		t.Fatal("custom model published")
	}
	if _, ok := byID["title-helper"]; ok {
		t.Fatal("helper model published")
	}
}

func TestFindModelResolvesPrefixAndMax(t *testing.T) {
	c := testCredential(siteTraeCN)
	c.UID = "models-find"
	sender := &fakeSender{reply: jsonReply(testListing)}
	if m, ok := findModel(sender, c, mustSite(t, siteTraeCN), "trae-cn/GLM-5.2"); !ok || m.ID != "glm-5.2" {
		t.Fatalf("model = %#v ok=%t", m, ok)
	}
	if m, ok := findModel(sender, c, mustSite(t, siteTraeCN), "glm-5.2-max"); !ok || m.Base != "glm-5.2" {
		t.Fatalf("max = %#v ok=%t", m, ok)
	}
	if m, ok := findModel(sender, c, mustSite(t, siteTraeCN), "deepseek-v4.1-flash"); !ok || m.Function != "solo_agent" {
		t.Fatalf("function = %#v ok=%t", m, ok)
	}
	if _, ok := findModel(sender, c, mustSite(t, siteTraeCN), "not-a-model"); ok {
		t.Fatal("unknown model resolved")
	}
}

func TestStaticModelsFollowConfiguredSite(t *testing.T) {
	t.Cleanup(func() { _ = configure(nil) })
	if err := configure([]byte("site: trae-cn")); err != nil {
		t.Fatal(err)
	}
	resp := staticModels()
	ids := map[string]bool{}
	for _, m := range resp.Models {
		ids[m.ID] = true
	}
	if !ids["glm-5.2"] || ids["gpt-5.4"] {
		t.Fatalf("ids = %#v", ids)
	}
}

func mustSite(t *testing.T, id string) siteSpec {
	t.Helper()
	spec, ok := siteFor(id)
	if !ok {
		t.Fatalf("unknown site %s", id)
	}
	return spec
}
