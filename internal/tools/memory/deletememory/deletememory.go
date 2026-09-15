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

package deletememory

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

const resourceType string = "delete-memory"

const defaultDescription = `Permanently delete a memory by memory_id (obtained from search_memory or create_memory). Use it when the user asks to forget something or when a memory is confirmed obsolete and should not simply be corrected with update_memory.

Only memories owned by the current user (or GLOBAL memories) can be deleted.`

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

// Config is the YAML configuration of a delete-memory tool.
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
		parameters.NewStringParameter("memory_id", "UUID of the memory to delete.", parameters.WithStringRequired(true)),
		cfg.UserIDParameter(),
	}

	return Tool{
		BaseTool: tools.NewBaseTool(
			cfg,
			tools.GetAnnotationsOrDefault(cfg.Annotations, tools.NewDestructiveAnnotations),
			tools.Manifest{Description: cfg.Description, Parameters: allParameters.Manifest(), AuthRequired: cfg.AuthRequired},
			allParameters,
		),
	}, nil
}

var _ tools.Tool = Tool{}

// Tool implements delete-memory.
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

	stmt := fmt.Sprintf(`DELETE FROM %s WHERE memory_id = $1::uuid AND %s RETURNING %s`, table, memory.ScopeClause("$2"), memory.Columns)
	rows, err := pool.Query(ctx, stmt, memoryID, userID)
	if err != nil {
		return nil, util.ProcessGeneralError(fmt.Errorf("unable to delete memory: %w", err))
	}
	deleted, err := memory.CollectMemories(rows, false)
	if err != nil {
		return nil, util.ProcessGeneralError(err)
	}
	if len(deleted) == 0 {
		return nil, util.NewAgentError(fmt.Sprintf("memory %q was not found or is not visible to the current user", memoryID), nil)
	}
	return Result{Status: "deleted", Message: "Memory deleted.", Memory: &deleted[0]}, nil
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
