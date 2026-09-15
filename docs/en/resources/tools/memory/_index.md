---
title: "Agent Memory"
type: docs
weight: 1
description: >
  Tools that give agents a persistent, per-user memory backed by Postgres + pgvector.
---

## About

The agent memory tools let an agent persist and recall discrete facts across
sessions: build fixes, coding-style preferences, project context, user
preferences, etc. Memories are stored in a Postgres table with a `pgvector`
embedding and are scoped to a `user_id`; each memory is either `PRIVATE` (only
visible to its owner) or `GLOBAL` (visible to every user of the memory store).

| tool type       | purpose                                                                                                   |
|-----------------|-----------------------------------------------------------------------------------------------------------|
| `create-memory` | Store a new fact. Runs synchronous duplicate/conflict detection first and bootstraps the table if needed. |
| `search-memory` | Semantic recall of memories visible to the current user; refreshes salience of returned rows.             |
| `update-memory` | Correct, re-categorise, re-scope or pin an existing memory (re-embeds when content changes).              |
| `delete-memory` | Permanently forget a memory.                                                                              |

Every tool requires a compatible **source**, an **embeddingModel** and,
optionally, an **authService** that binds `user_id` from the caller's ID token.

## Compatible Sources

- [postgres](../../sources/postgres.md)
- [alloydb-postgres](../../sources/alloydb-pg.md)
- [cloud-sql-postgres](../../sources/cloud-sql-pg.md)

The database must have the `vector` extension available (the tools run
`CREATE EXTENSION IF NOT EXISTS vector` on first write).

## Schema

`create-memory` creates the following table (and an HNSW cosine index plus a
`(user_id, category)` index) on first use:

```sql
CREATE TABLE IF NOT EXISTS mcp_agent_memories (
    memory_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id VARCHAR(255) NOT NULL,
    visibility VARCHAR(32) NOT NULL DEFAULT 'PRIVATE', -- PRIVATE or GLOBAL
    category VARCHAR(64) NOT NULL,                     -- build_fix, coding_style, ...
    content TEXT NOT NULL,                             -- the discrete fact
    embedding VECTOR(768) NOT NULL,
    is_pinned BOOLEAN DEFAULT FALSE,                   -- bypasses decay
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    last_accessed_at TIMESTAMPTZ DEFAULT NOW(),
    access_count INT DEFAULT 1
);
```

The embedding model must produce 768-dimensional vectors (e.g.
`gemini-embedding-001` with `dimension: 768`).

## Collision handling

`create-memory` performs a similarity search before inserting:

| top similarity                            | result                                                                                                                                     |
|-------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------|
| `>= duplicateThreshold` (default `0.95`)  | `status: duplicate` – nothing inserted; the existing memory's `access_count`/`last_accessed_at` are refreshed and it is returned.            |
| `>= conflictThreshold` (default `0.80`)   | `status: conflict` – nothing inserted; the similar memories are returned so the agent can call `update_memory` or retry with `force: true`. |
| otherwise                                 | `status: created`.                                                                                                                         |

## Example

```yaml
kind: source
name: toolbox-memory-db
type: postgres
host: 127.0.0.1
port: 5432
database: postgres
user: ${PG_USER}
password: ${PG_PASSWORD}
---
kind: embeddingModel
name: toolbox-embeddings
type: gemini
model: gemini-embedding-001
apiKey: ${GOOGLE_API_KEY}
dimension: 768
---
kind: authService
name: toolbox-auth
type: google
clientId: ${GOOGLE_CLIENT_ID}
---
kind: tool
name: create_memory
type: create-memory
source: toolbox-memory-db
embeddingModel: toolbox-embeddings
authService: toolbox-auth
---
kind: tool
name: search_memory
type: search-memory
source: toolbox-memory-db
embeddingModel: toolbox-embeddings
authService: toolbox-auth
---
kind: tool
name: update_memory
type: update-memory
source: toolbox-memory-db
embeddingModel: toolbox-embeddings
authService: toolbox-auth
---
kind: tool
name: delete_memory
type: delete-memory
source: toolbox-memory-db
embeddingModel: toolbox-embeddings
authService: toolbox-auth
---
kind: toolset
name: agent_memory
tools:
  - search_memory
  - create_memory
  - update_memory
  - delete_memory
```

Omit `authService` for local development: `user_id` then becomes an optional
tool parameter that defaults to `defaultUserId` (`"default"`).

## Reference

Fields shared by all four tool types:

| **field**      | **type** | **required** | **description**                                                                                              |
|----------------|:--------:|:------------:|--------------------------------------------------------------------------------------------------------------|
| type           |  string  |     true     | One of `create-memory`, `search-memory`, `update-memory`, `delete-memory`.                                   |
| source         |  string  |     true     | Name of a compatible Postgres source.                                                                        |
| embeddingModel |  string  |     true     | Name of the embedding model (768 dimensions) used for content and queries.                                   |
| authService    |  string  |    false     | Auth service whose verified token supplies `user_id`. If unset, `user_id` is an optional tool parameter.     |
| userIdField    |  string  |    false     | Token claim used as `user_id` (default `sub`). Use `email` for human-readable IDs.                           |
| defaultUserId  |  string  |    false     | `user_id` used when no authService is configured and none is passed (default `default`).                    |
| tableName      |  string  |    false     | Table to use, optionally schema-qualified (default `mcp_agent_memories`).                                   |
| description    |  string  |    false     | Overrides the built-in tool description.                                                                     |

`create-memory` extras:

| **field**          | **type** | **required** | **description**                                                     |
|--------------------|:--------:|:------------:|---------------------------------------------------------------------|
| duplicateThreshold |  float   |    false     | Similarity treated as a duplicate (default `0.95`).                 |
| conflictThreshold  |  float   |    false     | Similarity treated as a potential conflict (default `0.80`).        |
| maxCandidates      |   int    |    false     | Max similar memories returned on conflict (default `5`).            |

`search-memory` extras:

| **field**     | **type** | **required** | **description**                                       |
|---------------|:--------:|:------------:|-------------------------------------------------------|
| defaultLimit  |   int    |    false     | Default number of results (default `5`, max `50`).    |
| minSimilarity |  float   |    false     | Minimum similarity to return a memory (default `0.5`).|

### Tool parameters

| tool            | parameters                                                                                             |
|-----------------|--------------------------------------------------------------------------------------------------------|
| `create_memory` | `content` (req), `category` (req), `visibility` (PRIVATE/GLOBAL), `is_pinned`, `force`, `user_id`*     |
| `search_memory` | `query` (req), `category`, `limit`, `user_id`*                                                         |
| `update_memory` | `memory_id` (req), `content`, `category`, `visibility`, `is_pinned`, `user_id`*                        |
| `delete_memory` | `memory_id` (req), `user_id`*                                                                          |

\* `user_id` is hidden from the model and bound from the ID token when
`authService` is configured.
