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

// Package memory contains the shared building blocks for the agent memory
// tools (create-memory, search-memory, update-memory, delete-memory).
//
// Memories are discrete "facts" stored in a Postgres table (pgvector) and
// scoped to a user_id. A memory is either PRIVATE (visible only to the owning
// user) or GLOBAL (visible to every user of the memory database).
package memory

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/googleapis/mcp-toolbox/internal/embeddingmodels"
	"github.com/googleapis/mcp-toolbox/internal/sources"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// DefaultTableName is the table used to persist memories when none is
	// configured.
	DefaultTableName = "mcp_agent_memories"
	// EmbeddingDimensions is the fixed dimensionality of the embedding column.
	EmbeddingDimensions = 768
	// DefaultUserIDField is the ID-token claim used as user_id when an
	// authService is configured.
	DefaultUserIDField = "sub"
	// DefaultUserID is used as user_id when no authService is configured.
	DefaultUserID = "default"

	VisibilityPrivate = "PRIVATE"
	VisibilityGlobal  = "GLOBAL"

	// DefaultDuplicateThreshold is the cosine similarity at or above which a
	// new memory is treated as a duplicate of an existing one.
	DefaultDuplicateThreshold = 0.95
	// DefaultConflictThreshold is the cosine similarity at or above which a
	// new memory is flagged as potentially conflicting/overlapping with an
	// existing one and returned to the agent for a decision.
	DefaultConflictThreshold = 0.80
)

// CompatibleSource is the minimal source surface the memory tools need. It is
// satisfied by the postgres, alloydb-postgres and cloud-sql-postgres sources.
type CompatibleSource interface {
	PostgresPool() *pgxpool.Pool
}

// Config holds the YAML fields shared by all four memory tools.
type Config struct {
	tools.ConfigBase `yaml:",inline"`
	Type             string `yaml:"type" validate:"required"`
	Source           string `yaml:"source" validate:"required"`
	// EmbeddingModel is the name of the embeddingModel primitive used to embed
	// memory content and search queries.
	EmbeddingModel string `yaml:"embeddingModel" validate:"required"`
	// AuthService, when set, binds user_id from the verified ID token claims of
	// this authService instead of accepting it as a tool parameter.
	AuthService string `yaml:"authService"`
	// UserIDField is the claim to read from the authService token. Defaults to
	// "sub".
	UserIDField string `yaml:"userIdField"`
	// DefaultUserID is the user_id applied when no authService is configured
	// and the caller doesn't pass one. Defaults to "default".
	DefaultUserID string `yaml:"defaultUserId"`
	// TableName optionally overrides the memory table (may be schema
	// qualified). Defaults to "mcp_agent_memories".
	TableName   string                 `yaml:"tableName"`
	Annotations *tools.ToolAnnotations `yaml:"annotations,omitempty"`
}

var tableNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)?$`)

// Resolve fills defaults and validates the shared config. It returns the
// resolved copy.
func (c Config) Resolve() (Config, error) {
	if c.EmbeddingModel == "" {
		return c, fmt.Errorf("tool %q: embeddingModel is required", c.Name)
	}
	if c.TableName == "" {
		c.TableName = DefaultTableName
	}
	if !tableNameRe.MatchString(c.TableName) {
		return c, fmt.Errorf("tool %q: invalid tableName %q (expected [schema.]table using only letters, digits and underscores)", c.Name, c.TableName)
	}
	if c.UserIDField == "" {
		c.UserIDField = DefaultUserIDField
	}
	if c.DefaultUserID == "" {
		c.DefaultUserID = DefaultUserID
	}
	return c, nil
}

// UserIDParameter builds the user_id parameter. When an authService is
// configured the value is bound server-side from token claims (and is hidden
// from the model); otherwise it is an optional string parameter with a
// default.
func (c Config) UserIDParameter() parameters.Parameter {
	if c.AuthService != "" {
		return parameters.NewStringParameter(
			"user_id",
			"Identity of the memory owner. Bound automatically from the authenticated user.",
			parameters.WithStringAuth([]parameters.ParamAuthService{{Name: c.AuthService, Field: c.UserIDField}}),
		)
	}
	return parameters.NewStringParameter(
		"user_id",
		"Identity of the memory owner. Omit to use the server default.",
		parameters.WithStringDefault(c.DefaultUserID),
	)
}

// EmbeddingParameter builds a hidden parameter whose value is copied from
// `sourceParam` and replaced by its embedding (pgvector literal) before Invoke.
func (c Config) EmbeddingParameter(name, sourceParam string) parameters.Parameter {
	p := parameters.NewStringParameter(name, fmt.Sprintf("Embedding of %q.", sourceParam))
	p.ValueFromParam = sourceParam
	p.EmbeddedBy = c.EmbeddingModel
	return p
}

// Validate checks that the tool's source is compatible.
func (c Config) Validate(source sources.Source) error {
	if _, ok := source.(CompatibleSource); !ok {
		return fmt.Errorf("invalid source for %q tool: source %q is not a compatible type (postgres, alloydb-postgres or cloud-sql-postgres required)", c.Type, c.Source)
	}
	return nil
}

// EmbedParams is a nil-tolerant variant of parameters.EmbedParams: parameters
// marked `embeddedBy` whose value is nil (e.g. an optional `content` that was
// not supplied to update-memory) are left untouched instead of erroring.
func EmbedParams(ctx context.Context, ps parameters.Parameters, paramValues parameters.ParamValues, pMgr tools.PrimitiveManagerI) (parameters.ParamValues, error) {
	type target struct {
		index int
		text  string
	}
	byModel := map[string][]target{}
	for i, p := range ps {
		model := p.GetEmbeddedBy()
		if model == "" || i >= len(paramValues) || paramValues[i].Value == nil {
			continue
		}
		text, ok := paramValues[i].Value.(string)
		if !ok {
			return nil, fmt.Errorf("parameter %q is marked for embedding but has a non-string value (type: %T)", p.GetName(), paramValues[i].Value)
		}
		if strings.TrimSpace(text) == "" {
			paramValues[i].Value = nil
			continue
		}
		byModel[model] = append(byModel[model], target{index: i, text: text})
	}
	for modelName, targets := range byModel {
		model, ok := pMgr.GetEmbeddingModel(modelName)
		if !ok {
			return nil, fmt.Errorf("embedding model does not exist: %s", modelName)
		}
		batch := make([]string, len(targets))
		for i, t := range targets {
			batch[i] = t.text
		}
		vectors, err := model.EmbedParameters(ctx, batch)
		if err != nil {
			return nil, fmt.Errorf("error embedding parameters with model %s: %w", modelName, err)
		}
		if len(vectors) != len(batch) {
			return nil, fmt.Errorf("model %s returned %d embeddings for %d inputs", modelName, len(vectors), len(batch))
		}
		for i, v := range vectors {
			if len(v) != EmbeddingDimensions {
				return nil, fmt.Errorf("model %s returned a %d-dimensional embedding; the memory table requires %d dimensions", modelName, len(v), EmbeddingDimensions)
			}
			paramValues[targets[i].index].Value = embeddingmodels.FormatVectorForPgvector(v)
		}
	}
	return paramValues, nil
}

// Schema returns the DDL statements that bootstrap the memory table.
func Schema(table string) []string {
	idxPrefix := strings.ReplaceAll(table, ".", "_")
	return []string{
		`CREATE EXTENSION IF NOT EXISTS vector`,
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
    memory_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id VARCHAR(255) NOT NULL,
    visibility VARCHAR(32) NOT NULL DEFAULT 'PRIVATE',
    category VARCHAR(64) NOT NULL,
    content TEXT NOT NULL,
    embedding VECTOR(%d) NOT NULL,
    is_pinned BOOLEAN DEFAULT FALSE,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    last_accessed_at TIMESTAMPTZ DEFAULT NOW(),
    access_count INT DEFAULT 1
)`, table, EmbeddingDimensions),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_embedding_idx ON %s USING hnsw (embedding vector_cosine_ops)`, idxPrefix, table),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_user_category_idx ON %s (user_id, category)`, idxPrefix, table),
	}
}

// TableEnsurer lazily creates the memory table (once per process per tool)
// the first time a write is attempted. Failures are retried on the next call.
type TableEnsurer struct {
	mu   sync.Mutex
	done bool
}

// NewTableEnsurer returns a TableEnsurer.
func NewTableEnsurer() *TableEnsurer { return &TableEnsurer{} }

// Ensure runs the schema DDL if it hasn't succeeded yet.
func (e *TableEnsurer) Ensure(ctx context.Context, pool *pgxpool.Pool, table string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.done {
		return nil
	}
	for _, stmt := range Schema(table) {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("unable to bootstrap memory table %q: %w (is the pgvector extension installed?)", table, err)
		}
	}
	e.done = true
	return nil
}

// Memory is the JSON representation of a stored memory returned to agents.
type Memory struct {
	MemoryID       string    `json:"memory_id"`
	UserID         string    `json:"user_id"`
	Visibility     string    `json:"visibility"`
	Category       string    `json:"category"`
	Content        string    `json:"content"`
	IsPinned       bool      `json:"is_pinned"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	LastAccessedAt time.Time `json:"last_accessed_at"`
	AccessCount    int32     `json:"access_count"`
	// Similarity is the cosine similarity to the query/candidate embedding
	// (1 = identical). Only populated by similarity queries.
	Similarity *float64 `json:"similarity,omitempty"`
}

// Columns is the SELECT list matching ScanMemory (without similarity).
const Columns = `memory_id, user_id, visibility, category, content, is_pinned, created_at, updated_at, last_accessed_at, access_count`

// ScanMemory scans a row produced with Columns (optionally followed by a
// similarity column when withSimilarity is true).
func ScanMemory(rows pgx.Rows, withSimilarity bool) (Memory, error) {
	var m Memory
	var id [16]byte
	var isPinned *bool
	var accessCount *int32
	dest := []any{&id, &m.UserID, &m.Visibility, &m.Category, &m.Content, &isPinned, &m.CreatedAt, &m.UpdatedAt, &m.LastAccessedAt, &accessCount}
	if withSimilarity {
		dest = append(dest, &m.Similarity)
	}
	if err := rows.Scan(dest...); err != nil {
		return m, err
	}
	m.MemoryID = formatUUID(id)
	if isPinned != nil {
		m.IsPinned = *isPinned
	}
	if accessCount != nil {
		m.AccessCount = *accessCount
	}
	return m, nil
}

// CollectMemories drains rows into a slice of Memory.
func CollectMemories(rows pgx.Rows, withSimilarity bool) ([]Memory, error) {
	defer rows.Close()
	out := []Memory{}
	for rows.Next() {
		m, err := ScanMemory(rows, withSimilarity)
		if err != nil {
			return nil, fmt.Errorf("unable to parse memory row: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("unable to read memories: %w", err)
	}
	return out, nil
}

// IsUndefinedTable reports whether err is Postgres' "relation does not exist"
// error (SQLSTATE 42P01), i.e. no memory has been created yet.
func IsUndefinedTable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42P01"
}

// Touch bumps the salience counters (access_count, last_accessed_at) of an
// existing memory and returns the refreshed row.
func Touch(ctx context.Context, pool *pgxpool.Pool, table, memoryID string) (Memory, error) {
	stmt := fmt.Sprintf(`UPDATE %s SET access_count = access_count + 1, last_accessed_at = NOW() WHERE memory_id = $1 RETURNING %s`, table, Columns)
	rows, err := pool.Query(ctx, stmt, memoryID)
	if err != nil {
		return Memory{}, fmt.Errorf("unable to refresh existing memory: %w", err)
	}
	ms, err := CollectMemories(rows, false)
	if err != nil {
		return Memory{}, err
	}
	if len(ms) != 1 {
		return Memory{}, fmt.Errorf("memory %q was not found", memoryID)
	}
	return ms[0], nil
}

func formatUUID(b [16]byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ScopeClause returns the SQL predicate restricting rows to those visible to
// the given user, where userParam is the positional placeholder (e.g. "$2").
func ScopeClause(userParam string) string {
	return fmt.Sprintf("(user_id = %s OR visibility = '%s')", userParam, VisibilityGlobal)
}

// NormalizeVisibility validates and upper-cases a visibility value.
func NormalizeVisibility(v string) (string, error) {
	switch strings.ToUpper(strings.TrimSpace(v)) {
	case "", VisibilityPrivate:
		return VisibilityPrivate, nil
	case VisibilityGlobal:
		return VisibilityGlobal, nil
	default:
		return "", fmt.Errorf("visibility must be %q or %q, got %q", VisibilityPrivate, VisibilityGlobal, v)
	}
}
