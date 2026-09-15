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

package creatememory_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/googleapis/mcp-toolbox/internal/server"
	"github.com/googleapis/mcp-toolbox/internal/testutils"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/tools/memory"
	"github.com/googleapis/mcp-toolbox/internal/tools/memory/creatememory"
)

func TestParseFromYaml(t *testing.T) {
	ctx, err := testutils.ContextWithNewLogger()
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	in := `
            kind: tool
            name: create_memory
            type: create-memory
            source: my-pg
            embeddingModel: my-embedder
            authService: my-auth
            userIdField: email
            duplicateThreshold: 0.9
	`
	dup := 0.9
	want := server.ToolConfigs{
		"create_memory": creatememory.Config{
			Config: memory.Config{
				ConfigBase:     tools.ConfigBase{Name: "create_memory", AuthRequired: []string{}},
				Type:           "create-memory",
				Source:         "my-pg",
				EmbeddingModel: "my-embedder",
				AuthService:    "my-auth",
				UserIDField:    "email",
			},
			DuplicateThreshold: &dup,
		},
	}
	_, _, _, got, _, _, _, _, err := server.UnmarshalPrimitiveConfig(ctx, testutils.FormatYaml(in))
	if err != nil {
		t.Fatalf("unable to unmarshal: %s", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("incorrect parse: diff %v", diff)
	}
}

func TestInitializeParameters(t *testing.T) {
	ctx, err := testutils.ContextWithNewLogger()
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	tcs := []struct {
		desc        string
		cfg         creatememory.Config
		wantVisible []string
		wantErr     bool
	}{
		{
			desc: "auth bound user_id is hidden from the manifest",
			cfg: creatememory.Config{Config: memory.Config{
				ConfigBase: tools.ConfigBase{Name: "create_memory"}, Type: "create-memory",
				Source: "pg", EmbeddingModel: "emb", AuthService: "auth",
			}},
			wantVisible: []string{"content", "category", "visibility", "is_pinned", "force", "user_id"},
		},
		{
			desc: "missing embedding model fails",
			cfg: creatememory.Config{Config: memory.Config{
				ConfigBase: tools.ConfigBase{Name: "create_memory"}, Type: "create-memory", Source: "pg",
			}},
			wantErr: true,
		},
		{
			desc: "bad table name fails",
			cfg: creatememory.Config{Config: memory.Config{
				ConfigBase: tools.ConfigBase{Name: "create_memory"}, Type: "create-memory", Source: "pg",
				EmbeddingModel: "emb", TableName: "drop table; --",
			}},
			wantErr: true,
		},
	}
	for _, tc := range tcs {
		t.Run(tc.desc, func(t *testing.T) {
			tool, err := tc.cfg.Initialize(ctx)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %s", err)
			}
			manifest, err := tool.Manifest(nil)
			if err != nil {
				t.Fatalf("unexpected error: %s", err)
			}
			var got []string
			for _, p := range manifest.Parameters {
				got = append(got, p.Name)
			}
			if diff := cmp.Diff(tc.wantVisible, got); diff != "" {
				t.Fatalf("unexpected manifest parameters: %s", diff)
			}
			// The hidden embedding parameter must be present internally.
			params, err := tool.GetParameters(nil)
			if err != nil {
				t.Fatalf("unexpected error: %s", err)
			}
			found := false
			for _, p := range params {
				if p.GetName() == "content_embedding" {
					found = true
					if p.GetEmbeddedBy() != "emb" || p.GetValueFromParam() != "content" {
						t.Fatalf("embedding param misconfigured: embeddedBy=%q valueFromParam=%q", p.GetEmbeddedBy(), p.GetValueFromParam())
					}
				}
			}
			if !found {
				t.Fatalf("content_embedding parameter missing")
			}
		})
	}
}
