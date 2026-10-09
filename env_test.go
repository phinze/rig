package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestEnvExports(t *testing.T) {
	home := t.TempDir()
	basedir := filepath.Join(home, "workspaces", "mir-75-add-zig-stack")
	if err := os.MkdirAll(filepath.Join(basedir, "runtime", "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(basedir, "tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(basedir, "cloud", ".iso"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The agent is set and deliberately absent from every want list below: rig
	// reads RIG_AGENT to seed the picker and must never write it, or a rig ends
	// up choosing for the next rig you create from inside it.
	m := manifest{ID: "mir-75", Title: "add zig stack", Agent: "codex", Repos: map[string]string{
		"runtime": "mirendev/runtime",
		"cloud":   "mirendev/cloud",
	}}
	if err := writeManifest(basedir, m); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		cwd  string
		want []string
	}{
		{
			"basedir itself", basedir,
			[]string{
				"export RIG_BASEDIR='" + basedir + "'",
				"PATH_rm '" + filepath.Join(basedir, ".rig", "bin") + "'",
				"unset GH_REPO",
				"export RIG_ID='mir-75'",
			},
		},
		{
			"repo workspace dir", filepath.Join(basedir, "runtime"),
			[]string{
				"export RIG_BASEDIR='" + basedir + "'",
				"PATH_rm '" + filepath.Join(basedir, ".rig", "bin") + "'",
				"unset GH_REPO",
				"export RIG_ID='mir-75'",
				"export RIG_WORKSPACE='mir-75-runtime'",
				"export RIG_PORT=17527",
			},
		},
		{
			"nested under repo workspace", filepath.Join(basedir, "runtime", "pkg"),
			[]string{
				"export RIG_BASEDIR='" + basedir + "'",
				"PATH_rm '" + filepath.Join(basedir, ".rig", "bin") + "'",
				"unset GH_REPO",
				"export RIG_ID='mir-75'",
				"export RIG_WORKSPACE='mir-75-runtime'",
				"export RIG_PORT=17527",
			},
		},
		{
			"non-repo subdir gets rig identity only", filepath.Join(basedir, "tmp"),
			[]string{
				"export RIG_BASEDIR='" + basedir + "'",
				"PATH_rm '" + filepath.Join(basedir, ".rig", "bin") + "'",
				"unset GH_REPO",
				"export RIG_ID='mir-75'",
			},
		},
		{
			"iso-using repo also gets ISO_SESSION", filepath.Join(basedir, "cloud"),
			[]string{
				"export RIG_BASEDIR='" + basedir + "'",
				"PATH_rm '" + filepath.Join(basedir, ".rig", "bin") + "'",
				"unset GH_REPO",
				"export RIG_ID='mir-75'",
				"export RIG_WORKSPACE='mir-75-cloud'",
				"export RIG_PORT=17314",
				"export ISO_SESSION='dev-mir-75-cloud'",
			},
		},
		{"outside everything", home, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := envExports(c.cwd); !reflect.DeepEqual(got, c.want) {
				t.Errorf("envExports(%q):\n got  %q\n want %q", c.cwd, got, c.want)
			}
		})
	}
}

func TestHashPort(t *testing.T) {
	for _, key := range []string{"", "mir-75-runtime", "mir-1-x", "a-very-long-workspace-name-indeed"} {
		p := hashPort(key)
		if p < 10000 || p > 19999 {
			t.Errorf("hashPort(%q) = %d, out of [10000,19999]", key, p)
		}
		if p2 := hashPort(key); p2 != p {
			t.Errorf("hashPort(%q) not stable: %d then %d", key, p, p2)
		}
	}
}
