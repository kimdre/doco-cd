package filesystem

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveSymlinkWithin(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	root := filepath.Join(base, "root")

	writeTestTree(t, root, map[string]string{"config/prod.conf": "prod", "dir/file": "file"})
	writeTestTree(t, base, map[string]string{"outside.conf": "outside"})

	for _, link := range [][2]string{
		{"app.conf", "config/prod.conf"},
		{"linked", "dir"},
		{"chain.conf", "app.conf"},
		{"escaping.conf", "../outside.conf"},
		{"dangling.conf", "config/missing.conf"},
		{"linked/app.conf", "../config/prod.conf"},
	} {
		if err := os.Symlink(link[1], filepath.Join(root, link[0])); err != nil {
			t.Fatal(err)
		}
	}

	testCases := []struct {
		name    string
		path    string
		want    string
		wantErr bool
	}{
		{name: "regular file", path: "config/prod.conf", want: "config/prod.conf"},
		{name: "directory", path: "dir", want: "dir"},
		{name: "missing path", path: "missing", want: "missing"},
		{name: "symlink to file", path: "app.conf", want: "config/prod.conf"},
		{name: "symlink to directory", path: "linked", want: "dir"},
		{name: "symlink chain", path: "chain.conf", want: "config/prod.conf"},
		{name: "symlink below a symlinked directory", path: "linked/app.conf", want: "config/prod.conf"},
		{name: "escaping symlink", path: "escaping.conf", wantErr: true},
		{name: "dangling symlink", path: "dangling.conf", wantErr: true},
	}

	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := ResolveSymlinkWithin(root, filepath.Join(root, tc.path))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ResolveSymlinkWithin() = %q, want error", got)
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			want := filepath.Join(root, tc.path)
			if tc.want != tc.path {
				want = filepath.Join(realRoot, tc.want)
			}

			if got != want {
				t.Errorf("ResolveSymlinkWithin() = %q, want %q", got, want)
			}
		})
	}
}
