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

package creatememory

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

const resourceType string = "create-memory"

const defaultDescription = `Persist a single, discrete fact about the user, their project or their preferences so it can be recalled in future sessions (e.g. "All timestamps in the orders DB are stored in EST", "User prefers tabs over spaces in Go", "Fix for build error X is Y").

Before inserting, the tool searches for existing memories that are semantically similar:
- status "created": the memory was stored.
- status "duplicate": an equivalent memory already exists; it was NOT re-created and its salience was refreshed instead.
- status "conflict": similar memories exist that may overlap or contradict the new fact. Nothing was stored. Compare them with the new content, then either call update_memory on the matching memory_id (to correct/refresh it) or call create_memory again with force=true if the new fact is genuinely distinct.

Keep content short, self-contained and in the third person. Use a stable snake_case category (e.g. build_fix, coding_style, project_context, user_preference).`

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

// Config is the YAML configuration of a create-memory tool.
type Config struct {
	memory.Config `yaml:",inline"`
	// DuplicateThreshold is the cosine similarity at or above which the new
	// memory is treated as a duplicate (default 0.95).
	DuplicateThreshold *float64 `yaml:"duplicateThreshold"`
	// ConflictThreshold is the cosine similarity at or above which the new
	// memory is flagged as a potential conflict (default 0.80). Set to 1 or
	// higher to disable collision handling entirely.
	ConflictThreshold *float64 `yaml:"conflictThreshold"`
	// MaxCandidates caps how many similar memories are returned on conflict
	// (default 5).
	MaxCandidates *int `yaml:"maxCandidates"`
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
	dup := memory.DefaultDuplicateThreshold
	if cfg.DuplicateThreshold != nil {
		dup = *cfg.DuplicateThreshold
	}
	conflict := memory.DefaultConflictThreshold
	if cfg.ConflictThreshold != nil {
		conflict = *cfg.ConflictThreshold
	}
	if conflict > dup {
		return nil, fmt.Errorf("tool %q: conflictThreshold (%v) must be <= duplicateThreshold (%v)", cfg.Name, conflict, dup)
	}
	maxCandidates := 5
	if cfg.MaxCandidates != nil && *cfg.MaxCandidates > 0 {
		maxCandidates = *cfg.MaxCandidates
	}

	allParameters := parameters.Parameters{
		parameters.NewStringParameter("content", "The fact to remember, as a short self-contained sentence.", parameters.WithStringRequired(true)),
		parameters.NewStringParameter("category", "Stable snake_case grouping for the fact, e.g. build_fix, coding_style, project_context, user_preference.", parameters.WithStringRequired(true)),
		parameters.NewStringParameter("visibility", "PRIVATE (default, only the current user can recall it) or GLOBAL (shared with every user of this memory store).", parameters.WithStringDefault(memory.VisibilityPrivate)),
		parameters.NewBooleanParameter("is_pinned", "Pin the memory so it is never pruned by decay.", parameters.WithBooleanDefault(false)),
		parameters.NewBooleanParameter("force", "Skip duplicate/conflict detection and insert unconditionally. Only use after reviewing the conflict candidates.", parameters.WithBooleanDefault(false)),
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
		table:              memory.NewTableEnsurer(),
		duplicateThreshold: dup,
		conflictThreshold:  conflict,
		maxCandidates:      maxCandidates,
	}, nil
}

var _ tools.Tool = Tool{}

// Tool implements create-memory.
type Tool struct {
	tools.BaseTool[Config]
	table              *memory.TableEnsurer
	duplicateThreshold float64
	conflictThreshold  float64
	maxCandidates      int
}

// Result is the response returned to the agent.
type Result struct {
	// Status is one of "created", "duplicate" or "conflict".
	Status string `json:"status"`
	// Message explains the status and what the agent should do next.
	Message string `json:"message"`
	// Memory is the stored (or refreshed duplicate) memory, when applicable.
	Memory *memory.Memory `json:"memory,omitempty"`
	// SimilarMemories lists potentially conflicting memories on "conflict".
	SimilarMemories []memory.Memory `json:"similar_memories,omitempty"`
}

func (t Tool) Invoke(ctx context.Context, s sources.Source, params parameters.ParamValues, accessToken tools.AccessToken) (any, util.ToolboxError) {
	source, ok := s.(memory.CompatibleSource)
	if !ok {
		return nil, util.NewClientServerError("source used is not compatible with the tool", http.StatusInternalServerError, nil)
	}
	pool := source.PostgresPool()
	table := t.Cfg.TableName
	p := params.AsMap()

	content := strings.TrimSpace(asString(p["content"]))
	if content == "" {
		return nil, util.NewAgentError("content must not be empty", nil)
	}
	category := strings.TrimSpace(asString(p["category"]))
	if category == "" {
		return nil, util.NewAgentError("category must not be empty", nil)
	}
	visibility, err := memory.NormalizeVisibility(asString(p["visibility"]))
	if err != nil {
		return nil, util.NewAgentError(err.Error(), nil)
	}
	userID := asString(p["user_id"])
	if userID == "" {
		return nil, util.NewClientServerError("user_id could not be resolved", http.StatusBadRequest, nil)
	}
	embedding, ok := p["content_embedding"].(string)
	if !ok || embedding == "" {
		return nil, util.NewClientServerError("content embedding was not generated; check the embeddingModel configuration", http.StatusInternalServerError, nil)
	}
	isPinned, _ := p["is_pinned"].(bool)
	force, _ := p["force"].(bool)

	if err := t.table.Ensure(ctx, pool, table); err != nil {
		return nil, util.NewClientServerError(err.Error(), http.StatusInternalServerError, err)
	}

	// Synchronous collision handling: look for semantically similar memories
	// the caller can already see before inserting.
	if !force && t.conflictThreshold < 1 {
		query := fmt.Sprintf(`SELECT %s, 1 - (embedding <=> $1::vector) AS similarity
FROM %s
WHERE %s AND 1 - (embedding <=> $1::vector) >= $3
ORDER BY embedding <=> $1::vector
LIMIT $4`, memory.Columns, table, memory.ScopeClause("$2"))
		rows, err := pool.Query(ctx, query, embedding, userID, t.conflictThreshold, t.maxCandidates)
		if err != nil {
			return nil, util.ProcessGeneralError(fmt.Errorf("unable to check for similar memories: %w", err))
		}
		similar, err := memory.CollectMemories(rows, true)
		if err != nil {
			return nil, util.ProcessGeneralError(err)
		}
		if len(similar) > 0 {
			top := similar[0]
			if top.Similarity != nil && *top.Similarity >= t.duplicateThreshold {
				// Refresh salience of the existing memory instead of duplicating it.
				refreshed, err := memory.Touch(ctx, pool, table, top.MemoryID)
				if err != nil {
					return nil, util.ProcessGeneralError(err)
				}
				refreshed.Similarity = top.Similarity
				return Result{
					Status:  "duplicate",
					Message: fmt.Sprintf("An equivalent memory already exists (similarity %.3f). It was not re-created; its access count and last_accessed_at were refreshed. Call update_memory with this memory_id if the wording should change.", *top.Similarity),
					Memory:  &refreshed,
				}, nil
			}
			return Result{
				Status:          "conflict",
				Message:         fmt.Sprintf("%d existing memories are similar to the new content and may overlap or contradict it. Nothing was stored. Review them: if one describes the same fact, call update_memory with its memory_id and the corrected content; if the new fact is distinct, call create_memory again with force=true.", len(similar)),
				SimilarMemories: similar,
			}, nil
		}
	}

	insert := fmt.Sprintf(`INSERT INTO %s (user_id, visibility, category, content, embedding, is_pinned)
VALUES ($1, $2, $3, $4, $5::vector, $6)
RETURNING %s`, table, memory.Columns)
	rows, err := pool.Query(ctx, insert, userID, visibility, category, content, embedding, isPinned)
	if err != nil {
		return nil, util.ProcessGeneralError(fmt.Errorf("unable to create memory: %w", err))
	}
	created, err := memory.CollectMemories(rows, false)
	if err != nil {
		return nil, util.ProcessGeneralError(err)
	}
	if len(created) != 1 {
		return nil, util.NewClientServerError("memory insert returned no row", http.StatusInternalServerError, nil)
	}
	return Result{Status: "created", Message: "Memory stored.", Memory: &created[0]}, nil
}

func asString(v any) string {
	s, _ := v.(string)
	return s
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
