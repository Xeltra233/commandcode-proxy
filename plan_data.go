package main

// 官方套餐访问控制数据表，逐项还原自 command-code CLI 1.53.0（dist/cli.mjs）：
//   modelMetaTable  = jr   模型 → {provider, billing category}
//   planAccessRules = Br  套餐 planId → 允许的计费类别 + 模型黑名单
//   modelAliasTable = vr  旧模型 id 别名（含日期后缀的旧 id 归一化）
// 评估逻辑 evaluateModelAccess 与 CLI 完全一致（见 plan_detect.go）。

type modelMeta struct {
	Provider string
	Category string // "premium" | "opensource"
}

var modelMetaTable = map[string]modelMeta{
	"claude-sonnet-5":                       {Provider: "anthropic", Category: "premium"},
	"claude-sonnet-4-6":                     {Provider: "anthropic", Category: "premium"},
	"claude-fable-5-1":                      {Provider: "anthropic", Category: "premium"},
	"claude-fable-5":                        {Provider: "anthropic", Category: "premium"},
	"claude-opus-5":                         {Provider: "anthropic", Category: "premium"},
	"claude-opus-4-8":                       {Provider: "anthropic", Category: "premium"},
	"claude-opus-4-7":                       {Provider: "anthropic", Category: "premium"},
	"claude-haiku-4-5-20251001":             {Provider: "anthropic", Category: "premium"},
	"gpt-5.6-sol":                           {Provider: "vercel-ai-gateway", Category: "opensource"},
	"gpt-5.6-terra":                         {Provider: "openrouter", Category: "premium"},
	"gpt-5.6-luna":                          {Provider: "openrouter", Category: "opensource"},
	"gpt-6-astra":                           {Provider: "openai", Category: "premium"},
	"gpt-5.5":                               {Provider: "openai", Category: "premium"},
	"gpt-5.4":                               {Provider: "openai", Category: "premium"},
	"gpt-5.3-codex":                         {Provider: "openai", Category: "premium"},
	"gpt-5.4-mini":                          {Provider: "openai", Category: "premium"},
	"google/gemini-3.5-flash":               {Provider: "vercel-ai-gateway", Category: "premium"},
	"google/gemini-3.1-flash-lite":          {Provider: "vercel-ai-gateway", Category: "premium"},
	"sakana/fugu-ultra":                     {Provider: "vercel-ai-gateway", Category: "premium"},
	"meta/muse-spark-1.1":                   {Provider: "vercel-ai-gateway", Category: "premium"},
	"meta/muse-spark-1.2":                   {Provider: "vercel-ai-gateway", Category: "opensource"},
	"meta/muse-spark-1.2-contributor":       {Provider: "vercel-ai-gateway", Category: "opensource"},
	"meta/muse-spark-1.3":                   {Provider: "vercel-ai-gateway", Category: "opensource"},
	"meta/muse-spark-1.3-contributor":       {Provider: "vercel-ai-gateway", Category: "opensource"},
	"xai/grok-4.5":                          {Provider: "vercel-ai-gateway", Category: "opensource"},
	"xai/grok-4.6":                          {Provider: "vercel-ai-gateway", Category: "opensource"},
	"google/gemini-3.7-flash":               {Provider: "openrouter", Category: "opensource"},
	"google/gemini-3.8-flash":               {Provider: "vercel-ai-gateway", Category: "opensource"},
	"z-ai/glm-5.3-flash":                    {Provider: "vercel-ai-gateway", Category: "opensource"},
	"tencent/hy4-preview":                   {Provider: "cai", Category: "opensource"},
	"tencent/hy3-paid":                      {Provider: "cai", Category: "opensource"},
	"tencent/Hy3":                           {Provider: "cai", Category: "opensource"},
	"MiniMaxAI/MiniMax-M3-Free":             {Provider: "cai", Category: "opensource"},
	"MiniMaxAI/MiniMax-M3":                  {Provider: "cai", Category: "opensource"},
	"MiniMaxAI/MiniMax-M2.7":                {Provider: "cai", Category: "opensource"},
	"minimax/minimax-m3-free":               {Provider: "cai", Category: "opensource"},
	"minimax/minimax-m2.7-free":             {Provider: "cai", Category: "opensource"},
	"MiniMaxAI/MiniMax-M2.5":                {Provider: "cai", Category: "opensource"},
	"deepseek/deepseek-v4-pro":              {Provider: "cai", Category: "opensource"},
	"deepseek/deepseek-v4-flash":            {Provider: "cai", Category: "opensource"},
	"deepseek/deepseek-v4.1-flash":          {Provider: "cai", Category: "opensource"},
	"moonshotai/Kimi-K3":                    {Provider: "cai", Category: "opensource"},
	"moonshotai/Kimi-K2.7-Code":             {Provider: "cai", Category: "opensource"},
	"moonshotai/Kimi-K2.7-Code-Highspeed":   {Provider: "cai", Category: "opensource"},
	"moonshotai/Kimi-K2.6":                  {Provider: "cai", Category: "opensource"},
	"moonshotai/Kimi-K2.5":                  {Provider: "cai", Category: "opensource"},
	"zai-org/GLM-5.3":                       {Provider: "cai", Category: "opensource"},
	"zai-org/GLM-5.2":                       {Provider: "cai", Category: "opensource"},
	"zai-org/GLM-5.2-Fast":                  {Provider: "cai", Category: "opensource"},
	"zai-org/GLM-5.1":                       {Provider: "cai", Category: "opensource"},
	"zai-org/GLM-5":                         {Provider: "cai", Category: "opensource"},
	"xiaomi/mimo-v2.5-pro":                  {Provider: "cai", Category: "opensource"},
	"xiaomi/mimo-v2.5":                      {Provider: "cai", Category: "opensource"},
	"Qwen/Qwen3.6-Max-Preview":              {Provider: "cai", Category: "opensource"},
	"Qwen/Qwen3.6-Plus":                     {Provider: "cai", Category: "opensource"},
	"Qwen/Qwen3.7-Max":                      {Provider: "cai", Category: "opensource"},
	"Qwen/Qwen3.7-Plus":                     {Provider: "cai", Category: "opensource"},
	"Qwen/Qwen3.8-Max-0902":                 {Provider: "cai", Category: "opensource"},
	"Qwen/Qwen3.8-Max":                      {Provider: "cai", Category: "opensource"},
	"Qwen/Qwen3.8-27B":                      {Provider: "cai", Category: "opensource"},
	"Qwen/Qwen3.8-Flash":                    {Provider: "cai", Category: "opensource"},
	"Qwen/Qwen3.7-Flash":                    {Provider: "cai", Category: "opensource"},
	"meituan/LongCat-2.0:free":              {Provider: "cai", Category: "opensource"},
	"inclusionai/ling-3.0-flash-sante:free": {Provider: "cai", Category: "opensource"},
	"stepfun/Step-3.7-Flash":                {Provider: "cai", Category: "opensource"},
	"stepfun/Step-3.5-Flash":                {Provider: "cai", Category: "opensource"},
	"nvidia/nemotron-3-ultra-550b-a55b":     {Provider: "cai", Category: "opensource"},
	"thinkingmachines/inkling":              {Provider: "cai", Category: "opensource"},
}

type planAccessRule struct {
	AllowedCategories []string
	BlockedModels     []string // "provider:modelId" 全名
}

var planAccessRules = map[string]planAccessRule{
	"individual-go":       {AllowedCategories: []string{"opensource"}, BlockedModels: []string{"vercel-ai-gateway:meta/muse-spark-1.2", "vercel-ai-gateway:meta/muse-spark-1.3", "vercel-ai-gateway:xai/grok-4.6", "openrouter:google/gemini-3.7-flash", "vercel-ai-gateway:google/gemini-3.8-flash", "vercel-ai-gateway:gpt-5.6-sol"}},
	"individual-goat":     {AllowedCategories: []string{"opensource"}},
	"individual-pro":      {AllowedCategories: []string{"premium", "opensource"}, BlockedModels: []string{"anthropic:claude-fable-5-1", "anthropic:claude-fable-5", "anthropic:claude-opus-5", "anthropic:claude-opus-4-8", "anthropic:claude-opus-4-7", "anthropic:claude-opus-4-6", "anthropic:claude-opus-4-5-20251101", "openai:gpt-6-astra", "vercel-ai-gateway:sakana/fugu-ultra"}},
	"individual-pro-v1":   {AllowedCategories: []string{"premium", "opensource"}, BlockedModels: []string{"anthropic:claude-fable-5-1", "anthropic:claude-fable-5", "anthropic:claude-opus-5", "anthropic:claude-opus-4-8", "anthropic:claude-opus-4-7", "anthropic:claude-opus-4-6", "anthropic:claude-opus-4-5-20251101", "openai:gpt-6-astra", "vercel-ai-gateway:sakana/fugu-ultra"}},
	"individual-provider": {AllowedCategories: []string{"premium", "opensource"}},
	"individual-max":      {AllowedCategories: []string{"premium", "opensource"}},
	"individual-ultra":    {AllowedCategories: []string{"premium", "opensource"}},
	"teams-pro":           {AllowedCategories: []string{"premium", "opensource"}},
}

// modelAliasTable：官方别名表（旧日期后缀 id → 现行 id）。
var modelAliasTable = map[string]string{
	"claude-sonnet-4-20250514":   "claude-sonnet-4-6",
	"claude-sonnet-4-5-20250929": "claude-sonnet-4-6",
	"claude-opus-4-5-20251101":   "claude-opus-4-7",
	"claude-opus-4-6":            "claude-opus-4-7",
	"claude-haiku-4-5":           "claude-haiku-4-5-20251001",
}
