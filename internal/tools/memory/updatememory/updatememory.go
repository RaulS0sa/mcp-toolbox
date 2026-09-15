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

package updatememory

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	yaml "github.com/goccy/go-yaml"
	"github.com/googleapis/mcp-toolbox/internal/sources"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/tools/memory"
	"github.com/googleapis/mcp-toolbox/internal/util"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
)

const resourceType string = "update-memory"

const defaultDescription = `Update an existing memory in place, identified by memory_id (obtained from search_memory or create_memory). Use it to correct a fact that changed or was wrong, or to change its visibility. Only the supplied fields are changed; content updates are re-embedded automatically.

Only memories owned by the current user (or GLOBAL memories) can be updated.`

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

// Config is the YAML configuration of an update-memory tool.
type Config struct {
	memory.Config `yaml:",inline"`
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

	allParameters := parameters.Parameters{
		parameters.NewStringParameter("memory_id", "UUID of the memory to update.", parameters.WithStringRequired(true)),
		parameters.NewStringParameter("content", "New content for the memory. Omit to keep the current content.", parameters.WithStringRequired(false)),
		parameters.NewStringParameter("visibility", "New visibility: PRIVATE or GLOBAL. Omit to keep the current visibility.", parameters.WithStringRequired(false)),
		cfg.UserIDParameter(),
		cfg.EmbeddingParameter("content_embedding", "content"),
	}

	return Tool{
		BaseTool: tools.NewBaseTool(
			cfg,
			tools.GetAnnotationsOrDefault(cfg.Annotations, tools.NewWriteAnnotations),
			tools.Manifest{Description: cfg.Description, Parameters: allParameters.Manifest(), AuthRequired: cfg.AuthRequired},
			allParameters,
		),
	}, nil
}

var _ tools.Tool = Tool{}

// Tool implements update-memory.
type Tool struct {
	tools.BaseTool[Config]
}

// Result is the response returned to the agent.
type Result struct {
	Status  string         `json:"status"`
	Message string         `json:"message"`
	Memory  *memory.Memory `json:"memory,omitempty"`
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

	memoryID, _ := p["memory_id"].(string)
	memoryID = strings.TrimSpace(memoryID)
	if memoryID == "" {
		return nil, util.NewAgentError("memory_id is required", nil)
	}
	userID, _ := p["user_id"].(string)
	if userID == "" {
		return nil, util.NewClientServerError("user_id could not be resolved", http.StatusBadRequest, nil)
	}

	var sets []string
	args := []any{memoryID, userID}
	changed := false
	add := func(col string, v any, cast string) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s = $%d%s", col, len(args), cast))
		changed = true
	}

	if content, ok := p["content"].(string); ok && strings.TrimSpace(content) != "" {
		embedding, ok := p["content_embedding"].(string)
		if !ok || embedding == "" {
			return nil, util.NewClientServerError("content embedding was not generated; check the embeddingModel configuration", http.StatusInternalServerError, nil)
		}
		add("content", strings.TrimSpace(content), "")
		add("embedding", embedding, "::vector")
	}
	if visibility, ok := p["visibility"].(string); ok && strings.TrimSpace(visibility) != "" {
		v, err := memory.NormalizeVisibility(visibility)
		if err != nil {
			return nil, util.NewAgentError(err.Error(), nil)
		}
		add("visibility", v, "")
	}

	if !changed {
		existing, err := memory.GetMemory(ctx, pool, table, memoryID)
		if err != nil {
			return nil, util.NewAgentError(fmt.Sprintf("memory %q was not found", memoryID), nil)
		}
		return Result{Status: "updated", Message: "No fields supplied; memory unchanged.", Memory: &existing}, nil
	}

	stmt := fmt.Sprintf(`UPDATE %s SET %s WHERE memory_id = $1::uuid AND %s RETURNING %s`, table, strings.Join(sets, ", "), memory.ScopeClause("$2"), memory.Columns)
	rows, err := pool.Query(ctx, stmt, args...)
	if err != nil {
		if logger != nil {
			logger.ErrorContext(ctx, fmt.Sprintf("update_memory: update query failed: %v", err))
		}
		return nil, util.ProcessGeneralError(fmt.Errorf("unable to update memory: %w", err))
	}
	updated, err := memory.CollectMemories(rows, false)
	if err != nil {
		if logger != nil {
			logger.ErrorContext(ctx, fmt.Sprintf("update_memory: reading updated row failed: %v", err))
		}
		return nil, util.ProcessGeneralError(err)
	}
	if len(updated) == 0 {
		return nil, util.NewAgentError(fmt.Sprintf("memory %q was not found or is not visible to the current user", memoryID), nil)
	}
	return Result{Status: "updated", Message: "Memory updated.", Memory: &updated[0]}, nil
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
