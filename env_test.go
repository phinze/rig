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
	m := manifest{ID: "mir-75", Title: "add zig stack", Repos: map[string]string{
		"runtime": "mirendev/runtime",
		"cloud":   "mirendev/cloud",
	}}
	if err := writeManifest(basedir, m); err != nil {
		t.Fatal(err)
	}

	legacy := filepath.Join(home, "workspaces", "github-com", "mirendev", "runtime", "mir-1224-some-slug")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
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
				"export RIG_ID='mir-75'",
			},
		},
		{
			"repo workspace dir", filepath.Join(basedir, "runtime"),
			[]string{
				"export RIG_BASEDIR='" + basedir + "'",
				"export RIG_ID='mir-75'",
				"export RIG_WORKSPACE='mir-75-runtime'",
				"export GH_REPO='mirendev/runtime'",
			},
		},
		{
			"nested under repo workspace", filepath.Join(basedir, "runtime", "pkg"),
			[]string{
				"export RIG_BASEDIR='" + basedir + "'",
				"export RIG_ID='mir-75'",
				"export RIG_WORKSPACE='mir-75-runtime'",
				"export GH_REPO='mirendev/runtime'",
			},
		},
		{
			"non-repo subdir gets rig identity only", filepath.Join(basedir, "tmp"),
			[]string{
				"export RIG_BASEDIR='" + basedir + "'",
				"export RIG_ID='mir-75'",
			},
		},
		{
			"iso-using repo also gets ISO_SESSION", filepath.Join(basedir, "cloud"),
			[]string{
				"export RIG_BASEDIR='" + basedir + "'",
				"export RIG_ID='mir-75'",
				"export RIG_WORKSPACE='mir-75-cloud'",
				"export ISO_SESSION='dev-mir-75-cloud'",
				"export GH_REPO='mirendev/cloud'",
			},
		},
		{
			"legacy layout", legacy,
			[]string{"export GH_REPO='mirendev/runtime'"},
		},
		{
			"legacy layout too shallow", filepath.Join(home, "workspaces", "github-com", "mirendev"),
			nil,
		},
		{"outside everything", home, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := envExports(c.cwd, home); !reflect.DeepEqual(got, c.want) {
				t.Errorf("envExports(%q):\n got  %q\n want %q", c.cwd, got, c.want)
			}
		})
	}
}
