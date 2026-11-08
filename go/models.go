package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// traeModelSeed is a catalogue entry the plugin ships for a site whose own
// listing cannot be read.
type traeModelSeed struct {
	ID      string
	Name    string
	Context int64
	Output  int64
	Images  bool
	Efforts []string
}

// traeFallbackCatalogue is what Trae's international chat is known to serve.
// The account's own listing takes over as soon as it is read.
var traeFallbackCatalogue = []traeModelSeed{
	{ID: "gpt-5.4", Name: "GPT-5.4", Context: 128_000, Images: true},
	{ID: "gemini-3-flash", Name: "Gemini 3 Flash", Context: 1_000_000, Images: true},
	{ID: "kimi-k2.5", Name: "Kimi K2.5", Context: 256_000, Images: true},
	{ID: "minimax-m2", Name: "MiniMax M2", Context: 128_000},
	{ID: "deepseek-v3.2", Name: "DeepSeek V3.2", Context: 128_000},
}

// traeCNFallbackCatalogue is what Trae CN's chat_v3 is known to serve.
var traeCNFallbackCatalogue = []traeModelSeed{
	{ID: "glm-5.2", Name: "GLM-5.2", Context: 200_000, Output: 32_000},
	{ID: "glm-5", Name: "GLM-5", Context: 128_000},
	{ID: "kimi-k2.6", Name: "Kimi K2.6", Context: 256_000, Images: true},
	{ID: "qwen-3.7-plus", Name: "Qwen 3.7 Plus", Context: 128_000, Images: true},
	{ID: "DeepSeek-V4-Pro", Name: "DeepSeek V4 Pro", Context: 128_000},
	{ID: "DeepSeek-V4-Flash", Name: "DeepSeek V4 Flash", Context: 128_000},
}

// traeMax is a model's Max mode: a bigger context window asked through a
// second, __max model.
type traeMax struct {
	Name      string
	Context   int64
	MaxTokens int64
}

// traeModel is one normalized listing entry.
type traeModel struct {
	// ID is Trae's config_name, and the name a client asks for.
	ID      string
	Name    string
	Context int64
	Output  int64
	Images  bool
	Efforts []string
	// Function is the chat function whose list names the model; a request goes
	// through it.
	Function string
	// DevName is the __dev model the function serves the model with, when its
	// list names one.
	DevName string
	DevMax  int64
	// Max is the model's Max mode, when its lists name one.
	Max *traeMax
	// Base is the model a Max is asked under.
	Base string
}

// listingEntry is one entry of a chat function's model list.
type listingEntry struct {
	ConfigName    string `json:"config_name"`
	Usage         string `json:"usage"`
	ConfigSwitch  *bool  `json:"config_switch"`
	IsCustomModel *bool  `json:"is_custom_model"`
	IsInvisible   *bool  `json:"is_invisible_to_user"`
	DisplayConfig struct {
		DisplayName   string `json:"display_name"`
		IsCustomModel *bool  `json:"is_custom_model"`
		Multimodal    *bool  `json:"multimodal"`
	} `json:"display_config"`
	DisplayName      string `json:"display_name"`
	DisplayModelName string `json:"display_model_name"`
	Multimodal       *bool  `json:"multimodal"`
	ContextWindow    struct {
		Dev int64 `json:"dev"`
		Max int64 `json:"max"`
	} `json:"context_window_tokens"`
	ContextWindowSize json.RawMessage `json:"context_window_size"`
	PromptMaxTokens   int64           `json:"prompt_max_tokens"`
	ModelDetailList   []struct {
		ModelName string `json:"model_name"`
		MaxTokens int64  `json:"max_tokens"`
	} `json:"model_detail_list"`
	ReasoningEffortConfig *struct {
		Options []string `json:"options"`
	} `json:"reasoning_effort_config"`
}

type functionGroup struct {
	Function       string         `json:"function"`
	ConfigInfoList []listingEntry `json:"config_info_list"`
}

type listingResponse struct {
	FunctionConfigs []functionGroup `json:"function_configs"`
	Data            *struct {
		FunctionConfigs []functionGroup `json:"function_configs"`
	} `json:"data"`
}

func (r listingResponse) groups() []functionGroup {
	if len(r.FunctionConfigs) > 0 {
		return r.FunctionConfigs
	}
	if r.Data != nil {
		return r.Data.FunctionConfigs
	}
	return nil
}

// chatFunctions are the IDE's chat functions, in the order a model is asked
// through: the classic IDE's agent, then SOLO's Work mode, then the TRAE agent,
// then SOLO Lite's agent, then SOLO's coder roster.
var chatFunctions = []string{"chat_v3", "solo_work_lite", "solo_agent", "solo_agent_lite", "solo_coder"}

// chatModel reports whether a list entry is a model to chat with. The lists
// also hold the IDE's own helpers, configs switched off, and custom model slots.
func chatModel(m listingEntry, spec siteSpec) bool {
	id := strings.TrimSpace(m.ConfigName)
	if id == "" || strings.HasPrefix(strings.ToLower(id), "custom_model") {
		return false
	}
	if m.Usage != "" && m.Usage != "chat_completion" {
		return false
	}
	if m.ConfigSwitch != nil && !*m.ConfigSwitch {
		return false
	}
	if m.IsCustomModel != nil && *m.IsCustomModel {
		return false
	}
	if m.DisplayConfig.IsCustomModel != nil && *m.DisplayConfig.IsCustomModel {
		return false
	}
	// The international picker hides Auto-mode routing channels and
	// pay-as-you-go twins; Trae CN's lists are served whole.
	if spec.ID == siteTrae && m.IsInvisible != nil && *m.IsInvisible {
		return false
	}
	return true
}

func modelNameOf(m listingEntry) string {
	return firstNonEmpty(m.DisplayConfig.DisplayName, m.DisplayName, m.DisplayModelName, m.ConfigName)
}

func devModel(m listingEntry) (name string, maxTokens int64) {
	for _, d := range m.ModelDetailList {
		if strings.HasSuffix(d.ModelName, "__dev") {
			return d.ModelName, d.MaxTokens
		}
	}
	return "", 0
}

func maxModel(m listingEntry) (string, int64) {
	for _, d := range m.ModelDetailList {
		if strings.HasSuffix(d.ModelName, "__max") {
			return d.ModelName, d.MaxTokens
		}
	}
	return "", 0
}

func contextOf(m listingEntry) int64 {
	if m.ContextWindow.Dev > 0 {
		return m.ContextWindow.Dev
	}
	if n := contextWindowSize(m.ContextWindowSize); n > 0 {
		return n
	}
	if m.ContextWindow.Max > 0 {
		return m.ContextWindow.Max
	}
	if m.PromptMaxTokens > 0 {
		return m.PromptMaxTokens
	}
	return 0
}

// contextWindowSize reads context_window_size, which Trae writes as a number,
// an array of numbers, or {max: ...}.
func contextWindowSize(raw json.RawMessage) int64 {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var nums []int64
	if json.Unmarshal(raw, &nums) == nil {
		for _, v := range nums {
			if v > 0 {
				return v
			}
		}
		return 0
	}
	var obj struct {
		Max json.RawMessage `json:"max"`
		Dev json.RawMessage `json:"dev"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		if n := contextWindowSize(obj.Dev); n > 0 {
			return n
		}
		return contextWindowSize(obj.Max)
	}
	return 0
}

func seesImages(m listingEntry) (bool, bool) {
	if m.Multimodal != nil {
		return *m.Multimodal, true
	}
	if m.DisplayConfig.Multimodal != nil {
		return *m.DisplayConfig.Multimodal, true
	}
	return false, false
}

// effortNames maps Trae's level names to the names OpenAI clients use.
var effortNames = map[string]string{
	"light":      "low",
	"medium":     "medium",
	"high":       "high",
	"extra_high": "xhigh",
	"max":        "max",
}

// effortsOf is an entry's levels in OpenAI's names, in Trae's order.
func effortsOf(m listingEntry) []string {
	if m.ReasoningEffortConfig == nil {
		return nil
	}
	var out []string
	for _, name := range m.ReasoningEffortConfig.Options {
		if mapped, ok := effortNames[strings.TrimSpace(name)]; ok {
			out = append(out, mapped)
		}
	}
	return out
}

// modelCache keeps one account's last list for a while, so a request does not
// pay for a list read every turn.
const modelCacheTTL = 5 * time.Minute

type cachedModels struct {
	at     time.Time
	models []traeModel
}

var modelCacheMu sync.Mutex
var modelCache = map[string]cachedModels{}

func cacheKey(spec siteSpec, c credential) string { return spec.ID + "\x00" + c.UID }

func cachedList(spec siteSpec, c credential) ([]traeModel, bool) {
	modelCacheMu.Lock()
	defer modelCacheMu.Unlock()
	entry, ok := modelCache[cacheKey(spec, c)]
	if !ok || time.Since(entry.at) > modelCacheTTL {
		return nil, false
	}
	return entry.models, true
}

func storeList(spec siteSpec, c credential, models []traeModel) {
	modelCacheMu.Lock()
	modelCache[cacheKey(spec, c)] = cachedModels{at: time.Now(), models: models}
	modelCacheMu.Unlock()
}

// fetchModels reads the account's live list, or answers the shipped catalogue
// when Trae cannot be asked.
func fetchModels(sender httpSender, c credential, spec siteSpec) []traeModel {
	if models, ok := cachedList(spec, c); ok {
		return models
	}
	models, err := liveModels(sender, c, spec)
	if err != nil || len(models) == 0 {
		return fallbackModels(spec)
	}
	storeList(spec, c, models)
	return models
}

// liveModels is the account's model list: every chat function's, since SOLO
// lists models the classic IDE's chat_v3 doesn't. One function's list failing
// leaves the others'; all failing is the error.
func liveModels(sender httpSender, c credential, spec siteSpec) ([]traeModel, error) {
	lists, err := batchLists(sender, c, spec)
	if err != nil {
		return nil, err
	}
	if lists == nil {
		lists = map[string][]listingEntry{}
		for _, fn := range chatFunctions {
			entries, err := oneList(sender, c, spec, fn)
			if err != nil {
				continue
			}
			lists[fn] = entries
		}
		if len(lists) == 0 {
			return nil, errors.New("Trae model lists unavailable")
		}
	}
	return assembleModels(lists, spec), nil
}

func batchLists(sender httpSender, c credential, spec siteSpec) (map[string][]listingEntry, error) {
	body, _ := json.Marshal(map[string]any{
		"functions":                 chatFunctions,
		"agent_type":                "",
		"current_config_info":       map[string]any{"config_name": "", "is_custom_model": false},
		"mode_type":                 0,
		"access_type":               0,
		"ab_force_vids":             "",
		"ab_autotest_advanced_mode": 0,
		"show_custom_model":         true,
	})
	resp, err := sender.Do(pluginapi.HTTPRequest{
		Method:  "POST",
		URL:     c.modelHost(spec) + "/api/ide/v1/batch_get_detail_param",
		Headers: ideHeaders(c, spec, map[string]string{"Accept": "application/json"}),
		Body:    body,
	})
	if err != nil {
		return nil, errors.New("Trae model list request failed")
	}
	if resp.StatusCode == 401 || lapsed(spec, errorCode(resp.Body)) {
		return nil, signInExpired(spec)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Trae model list: HTTP %d", resp.StatusCode)
	}
	var parsed listingResponse
	if json.Unmarshal(resp.Body, &parsed) != nil {
		return nil, errors.New("Trae model list: invalid answer")
	}
	groups := parsed.groups()
	if len(groups) == 0 {
		return nil, nil
	}
	out := map[string][]listingEntry{}
	for _, g := range groups {
		fn := strings.TrimSpace(g.Function)
		if !knownFunction(fn) || len(g.ConfigInfoList) == 0 {
			continue
		}
		out[fn] = append(out[fn], g.ConfigInfoList...)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func oneList(sender httpSender, c credential, spec siteSpec, fn string) ([]listingEntry, error) {
	body, _ := json.Marshal(map[string]any{
		"function": fn, "config_names": nil, "need_prompt": false, "current_config_info": nil,
		"poly_prompt": true, "mode_type": nil, "agent_type": nil,
	})
	resp, err := sender.Do(pluginapi.HTTPRequest{
		Method:  "POST",
		URL:     c.modelHost(spec) + "/api/ide/v1/get_detail_param",
		Headers: ideHeaders(c, spec, map[string]string{"Accept": "application/json"}),
		Body:    body,
	})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == 401 || lapsed(spec, errorCode(resp.Body)) {
		return nil, signInExpired(spec)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Trae model list: HTTP %d", resp.StatusCode)
	}
	var parsed struct {
		ConfigInfoList []listingEntry `json:"config_info_list"`
		Data           *struct {
			ConfigInfoList []listingEntry `json:"config_info_list"`
		} `json:"data"`
	}
	if json.Unmarshal(resp.Body, &parsed) != nil {
		return nil, errors.New("Trae model list: invalid answer")
	}
	if len(parsed.ConfigInfoList) > 0 {
		return parsed.ConfigInfoList, nil
	}
	if parsed.Data != nil {
		return parsed.Data.ConfigInfoList, nil
	}
	return nil, nil
}

func knownFunction(fn string) bool {
	for _, f := range chatFunctions {
		if f == fn {
			return true
		}
	}
	return false
}

// assembleModels merges the functions' lists. A model several functions list is
// asked through the first whose entry names a __dev model and isn't misnamed,
// else the first that names one, else the first.
func assembleModels(lists map[string][]listingEntry, spec siteSpec) []traeModel {
	names := map[string]map[string]int{}
	for _, fn := range chatFunctions {
		for _, m := range lists[fn] {
			if !chatModel(m, spec) {
				continue
			}
			id := m.ConfigName
			if names[id] == nil {
				names[id] = map[string]int{}
			}
			names[id][modelNameOf(m)]++
		}
	}
	agreed := map[string]string{}
	for id, by := range names {
		top, name := 0, ""
		tie := false
		for n, count := range by {
			switch {
			case count > top:
				top, name, tie = count, n, false
			case count == top:
				tie = true
			}
		}
		if !tie {
			agreed[id] = name
		}
	}
	misnamed := func(fn string, m listingEntry) bool {
		id, name := m.ConfigName, modelNameOf(m)
		if len(names[id]) <= 1 {
			return false
		}
		for _, o := range lists[fn] {
			if chatModel(o, spec) && o.ConfigName != id && modelNameOf(o) == name {
				return true
			}
		}
		return false
	}
	worth := func(fn string, m listingEntry) int {
		dev, _ := devModel(m)
		score := 0
		if dev != "" {
			score += 2
		}
		if !misnamed(fn, m) {
			score++
		}
		if modelNameOf(m) == agreed[m.ConfigName] {
			score++
		}
		return score*2 + boolInt(len(effortsOf(m)) > 0)
	}
	type chosen struct {
		entry listingEntry
		fn    string
		worth int
	}
	best := map[string]chosen{}
	order := []string{}
	for _, fn := range chatFunctions {
		for _, m := range lists[fn] {
			if !chatModel(m, spec) {
				continue
			}
			w := worth(fn, m)
			was, ok := best[m.ConfigName]
			if !ok {
				order = append(order, m.ConfigName)
			}
			if !ok || w > was.worth {
				best[m.ConfigName] = chosen{entry: m, fn: fn, worth: w}
			}
		}
	}
	var out []traeModel
	for _, id := range order {
		pick := best[id]
		m := pick.entry
		model := traeModel{
			ID:       id,
			Name:     modelNameOf(m),
			Context:  contextOf(m),
			Images:   true,
			Efforts:  effortsOf(m),
			Function: pick.fn,
		}
		if sees, ok := seesImages(m); ok {
			model.Images = sees
		}
		if model.Context <= 0 {
			model.Context = 128_000
		}
		if dev, most := devModel(m); dev != "" {
			model.DevName, model.DevMax = dev, most
			if most > 0 {
				model.Output = most
			}
		}
		// The Max is asked through the function whose entry names the __max
		// model, with the biggest window.
		var maxEntry *listingEntry
		var maxFn string
		for _, fn := range chatFunctions {
			for i := range lists[fn] {
				o := &lists[fn][i]
				if !chatModel(*o, spec) || o.ConfigName != id {
					continue
				}
				if name, most := maxModel(*o); name != "" {
					if maxEntry == nil || windowOf(*o) > windowOf(*maxEntry) {
						maxEntry, maxFn = o, fn
					}
					_ = most
				}
			}
		}
		if maxEntry != nil {
			name, most := maxModel(*maxEntry)
			window := windowOf(*maxEntry)
			if window > model.Context {
				model.Max = &traeMax{Name: name, Context: window, MaxTokens: most}
				maxID := id + "-max"
				out = append(out, traeModel{
					ID: maxID, Name: model.Name + " Max", Context: window,
					Output: firstNonZero(most, model.Output), Images: model.Images,
					Efforts: model.Efforts, Function: maxFn, Base: id,
					DevName: name, DevMax: most,
					Max: &traeMax{Name: name, Context: window, MaxTokens: most},
				})
			}
		}
		out = append(out, model)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func windowOf(m listingEntry) int64 {
	if m.ContextWindow.Max > 0 {
		return m.ContextWindow.Max
	}
	return contextWindowSize(m.ContextWindowSize)
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func firstNonZero(values ...int64) int64 {
	for _, v := range values {
		if v > 0 {
			return v
		}
	}
	return 0
}

func fallbackModels(spec siteSpec) []traeModel {
	out := make([]traeModel, 0, len(spec.Fallback))
	for _, seed := range spec.Fallback {
		out = append(out, traeModel{
			ID: seed.ID, Name: seed.Name, Context: seed.Context, Output: seed.Output,
			Images: seed.Images, Efforts: seed.Efforts, Function: chatFunctions[0],
		})
	}
	return out
}

// modelInfo is a model as CPA publishes it.
func modelInfo(m traeModel) pluginapi.ModelInfo {
	info := pluginapi.ModelInfo{
		ID:                         m.ID,
		Name:                       m.Name,
		DisplayName:                m.Name,
		Object:                     "model",
		OwnedBy:                    credentialProvider,
		Type:                       credentialProvider,
		ContextLength:              m.Context,
		InputTokenLimit:            m.Context,
		OutputTokenLimit:           m.Output,
		SupportedInputModalities:   []string{"text"},
		SupportedOutputModalities:  []string{"text"},
		SupportedGenerationMethods: []string{"chat.completions"},
	}
	if m.Images {
		info.SupportedInputModalities = []string{"text", "image"}
	}
	if len(m.Efforts) > 0 {
		info.Thinking = &pluginapi.ThinkingSupport{Levels: append([]string(nil), m.Efforts...)}
	}
	return info
}

func modelResponse(models []traeModel) pluginapi.ModelResponse {
	out := pluginapi.ModelResponse{Provider: credentialProvider, Models: make([]pluginapi.ModelInfo, 0, len(models))}
	for _, m := range models {
		if strings.TrimSpace(m.ID) == "" {
			continue
		}
		out.Models = append(out.Models, modelInfo(m))
	}
	return out
}

// staticModels publishes the shipped catalogue for the configured fallback site.
func staticModels() pluginapi.ModelResponse {
	spec, ok := siteFor(getConfig().Site)
	if !ok {
		spec, _ = siteFor(siteTrae)
	}
	return modelResponse(fallbackModels(spec))
}

func modelsForAuth(req pluginapi.AuthModelRequest, sender httpSender) (pluginapi.ModelResponse, error) {
	c, err := readCredential(req.StorageJSON)
	if err != nil {
		return pluginapi.ModelResponse{}, err
	}
	spec, ok := siteFor(c.Site)
	if !ok {
		return pluginapi.ModelResponse{}, errors.New("invalid Trae credential")
	}
	return modelResponse(fetchModels(sender, c, spec)), nil
}

// findModel resolves a client's model name: the id, a display name, or the id
// with a provider prefix.
func findModel(sender httpSender, c credential, spec siteSpec, model string) (traeModel, bool) {
	model = trimModelPrefix(model)
	for _, m := range fetchModels(sender, c, spec) {
		if matchesModel(m, model) {
			return m, true
		}
	}
	for _, m := range fallbackModels(spec) {
		if matchesModel(m, model) {
			return m, true
		}
	}
	return traeModel{}, false
}

func matchesModel(m traeModel, want string) bool {
	want = strings.TrimSpace(want)
	if want == "" {
		return false
	}
	if strings.EqualFold(m.ID, want) || strings.EqualFold(m.Name, want) {
		return true
	}
	return m.Base != "" && strings.EqualFold(m.Base+"-max", want)
}

var modelPrefix = regexp.MustCompile(`^[a-z0-9-]+/(.+)$`)

func trimModelPrefix(model string) string {
	if m := modelPrefix.FindStringSubmatch(strings.TrimSpace(model)); m != nil {
		return m[1]
	}
	return strings.TrimSpace(model)
}

// unknownModelMessage names the models an account could ask for instead.
func unknownModelMessage(sender httpSender, c credential, spec siteSpec, model string) string {
	ids := make([]string, 0, 16)
	for _, m := range fetchModels(sender, c, spec) {
		ids = append(ids, m.ID)
	}
	sort.Strings(ids)
	message := fmt.Sprintf("model %q is not available from %s now", model, spec.Realm)
	if len(ids) > 0 {
		message += "; models: " + strings.Join(ids, ", ")
	}
	return message
}

// catalogueURL is the endpoint the list is read from, exposed for tests.
func catalogueURL(spec siteSpec, c credential) string {
	u, _ := url.Parse(c.modelHost(spec) + "/api/ide/v1/batch_get_detail_param")
	return u.String()
}
