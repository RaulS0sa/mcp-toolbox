// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package searchmemory

import (
	"context"
	"fmt"
	"net/http"

	yaml "github.com/goccy/go-yaml"
	"github.com/googleapis/mcp-toolbox/internal/sources"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/tools/memory"
	"github.com/googleapis/mcp-toolbox/internal/util"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
)

const resourceType string = "search-memory"

const defaultDescription = `Semantically search persistent memories (facts, preferences, project context, known fixes) that were saved in earlier sessions. Call this at the start of a task, before answering questions about the user's environment or preferences, and before creating a new memory to check whether one already exists.

Returns the most relevant memories visible to the current user (their PRIVATE memories plus GLOBAL ones), ordered by similarity (1 = identical).`

func init() {
	if !tools.Register(resourceType, newConfig) {
		panic(fmt.Sprintf("tool type %q already registered", resourceType))
	}
}

func newConfig(ctx context.Context, name string, decoder *yaml.Decoder) (tools.ToolConfig, error) {
	actual := Config{Config: memory.Config{ConfigBase: tools.ConfigBase{Name: name}}}
	if err := decoder.DecodeContext(ctx, &actual); err != nil {
		return nil, err
	}
	return actual, nil
}

// Config is the YAML configuration of a search-memory tool.
type Config struct {
	memory.Config `yaml:",inline"`
	// DefaultLimit is the number of memories returned when the agent doesn't
	// specify one (default 5).
	DefaultLimit *int `yaml:"defaultLimit"`
	// MinSimilarity filters out weak matches (default 0.5).
	MinSimilarity *float64 `yaml:"minSimilarity"`
}

var _ tools.ToolConfig = Config{}

func (cfg Config) ToolConfigType() string {
	return resourceType
}

func (cfg Config) Initialize(context.Context) (tools.Tool, error) {
	resolved, err := cfg.Config.Resolve()
	if err != nil {
		return nil, err
	}
	cfg.Config = resolved
	if cfg.Description == "" {
		cfg.Description = defaultDescription
	}
	limit := 5
	if cfg.DefaultLimit != nil && *cfg.DefaultLimit > 0 {
		limit = *cfg.DefaultLimit
	}
	minSim := 0.5
	if cfg.MinSimilarity != nil {
		minSim = *cfg.MinSimilarity
	}
	one, fifty := 1, 50

	allParameters := parameters.Parameters{
		parameters.NewStringParameter("query", "Natural language description of what to recall, e.g. \"timezone of the orders database\".", parameters.WithStringRequired(true)),
		parameters.NewIntParameter("limit", "Maximum number of memories to return.", parameters.WithIntDefault(limit), parameters.WithIntMinValue(&one), parameters.WithIntMaxValue(&fifty)),
		cfg.UserIDParameter(),
		cfg.EmbeddingParameter("query_embedding", "query"),
	}

	return Tool{
		BaseTool: tools.NewBaseTool(
			cfg,
			tools.GetAnnotationsOrDefault(cfg.Annotations, tools.NewReadOnlyAnnotations),
			tools.Manifest{Description: cfg.Description, Parameters: allParameters.Manifest(), AuthRequired: cfg.AuthRequired},
			allParameters,
		),
		minSimilarity: minSim,
	}, nil
}

var _ tools.Tool = Tool{}

// Tool implements search-memory.
type Tool struct {
	tools.BaseTool[Config]
	minSimilarity float64
}

// Result is the response returned to the agent.
type Result struct {
	Memories []memory.Memory `json:"memories"`
	Message  string          `json:"message,omitempty"`
}

func (t Tool) Invoke(ctx context.Context, s sources.Source, params parameters.ParamValues, accessToken tools.AccessToken) (any, util.ToolboxError) {
	logger, _ := util.LoggerFromContext(ctx)
	source, ok := s.(memory.CompatibleSource)
	if !ok {
		return nil, util.NewClientServerError("source used is not compatible with the tool", http.StatusInternalServerError, nil)
	}
	pool := source.PostgresPool()
	table := t.Cfg.TableName
	p := params.AsMap()

	embedding, ok := p["query_embedding"].(string)
	if !ok || embedding == "" {
		return nil, util.NewClientServerError("query embedding was not generated; check the embeddingModel configuration", http.StatusInternalServerError, nil)
	}
	userID, _ := p["user_id"].(string)
	if userID == "" {
		return nil, util.NewClientServerError("user_id could not be resolved", http.StatusBadRequest, nil)
	}
	limit := 5
	if v, ok := p["limit"].(int); ok && v > 0 {
		limit = v
	}

	query := fmt.Sprintf(`SELECT %s, 1 - (embedding <=> $1::vector) AS similarity
FROM %s
WHERE %s AND 1 - (embedding <=> $1::vector) >= $3
ORDER BY embedding <=> $1::vector
LIMIT $4`, memory.Columns, table, memory.ScopeClause("$2"))

	rows, err := pool.Query(ctx, query, embedding, userID, t.minSimilarity, limit)
	if err != nil {
		if memory.IsUndefinedTable(err) {
			return Result{Memories: []memory.Memory{}, Message: "No memories have been stored yet."}, nil
		}
		if logger != nil {
			logger.ErrorContext(ctx, fmt.Sprintf("search_memory: query failed: %v", err))
		}
		return nil, util.ProcessGeneralError(fmt.Errorf("unable to search memories: %w", err))
	}
	memories, err := memory.CollectMemories(rows, true)
	if err != nil {
		if logger != nil {
			logger.ErrorContext(ctx, fmt.Sprintf("search_memory: collect rows failed: %v", err))
		}
		return nil, util.ProcessGeneralError(err)
	}
	res := Result{Memories: memories}
	if len(memories) == 0 {
		res.Message = "No relevant memories found."
	}
	return res, nil
}

func (t Tool) EmbedParams(ctx context.Context, paramValues parameters.ParamValues, pMgr tools.PrimitiveManagerI) (parameters.ParamValues, error) {
	return memory.EmbedParams(ctx, t.StaticParameters, paramValues, pMgr)
}

func (t Tool) GetSourceName() string {
	return t.Cfg.Source
}

func (t Tool) ToConfig() tools.ToolConfig {
	return t.Cfg
}

func (t Tool) ValidateSource(source sources.Source) error {
	return t.Cfg.Validate(source)
}
