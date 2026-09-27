package filesystem

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestTree(t *testing.T, root string, files map[string]string) {
	t.Helper()

	for name, content := range files {
		path := filepath.Join(root, name)

		if strings.HasSuffix(name, "/") {
			if err := os.MkdirAll(path, PermDir); err != nil {
				t.Fatal(err)
			}

			continue
		}

		if err := os.MkdirAll(filepath.Dir(path), PermDir); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte(content), PermPublic); err != nil {
			t.Fatal(err)
		}
	}
}

func TestContentEqual(t *testing.T) {
	t.Parallel()

	largeContent := strings.Repeat("x", 3*contentCompareBufferSize+17)

	testCases := []struct {
		name   string
		a, b   map[string]string
		path   string
		modify func(t *testing.T, a, b string)
		want   bool
	}{
		{
			name: "equal trees",
			a:    map[string]string{"dir/a.txt": "a", "dir/sub/b.txt": "b", "dir/empty/": ""},
			b:    map[string]string{"dir/a.txt": "a", "dir/sub/b.txt": "b", "dir/empty/": ""},
			path: "dir",
			want: true,
		},
		{
			name: "equal large files",
			a:    map[string]string{"file": largeContent},
			b:    map[string]string{"file": largeContent},
			path: "file",
			want: true,
		},
		{
			name: "different content with same size",
			a:    map[string]string{"dir/a.txt": "aaa"},
			b:    map[string]string{"dir/a.txt": "aab"},
			path: "dir",
			want: false,
		},
		{
			name: "different large content",
			a:    map[string]string{"file": largeContent + "a"},
			b:    map[string]string{"file": largeContent + "b"},
			path: "file",
			want: false,
		},
		{
			name: "additional file",
			a:    map[string]string{"dir/a.txt": "a"},
			b:    map[string]string{"dir/a.txt": "a", "dir/b.txt": "b"},
			path: "dir",
			want: false,
		},
		{
			name: "file and directory",
			a:    map[string]string{"entry": "a"},
			b:    map[string]string{"entry/": ""},
			path: "entry",
			want: false,
		},
		{
			name: "missing on both sides",
			path: "missing",
			want: true,
		},
		{
			name: "missing and directory tree without files",
			a:    map[string]string{"data/nested/mountpoint/": ""},
			path: "data",
			want: true,
		},
		{
			name: "missing and nested mount point",
			a:    map[string]string{"dir/a.txt": "a", "dir/node_modules/": ""},
			b:    map[string]string{"dir/a.txt": "a"},
			path: "dir",
			want: true,
		},
		{
			name: "missing and directory with files",
			a:    map[string]string{"data/file": "written by a container"},
			path: "data",
			want: false,
		},
		{
			name: "missing and file",
			b:    map[string]string{"file": ""},
			path: "file",
			want: false,
		},
		{
			name: "different permissions",
			a:    map[string]string{"script.sh": "echo"},
			b:    map[string]string{"script.sh": "echo"},
			path: "script.sh",
			modify: func(t *testing.T, _, b string) {
				t.Helper()

				if err := os.Chmod(filepath.Join(b, "script.sh"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: false,
		},
		{
			name: "equal symlinks",
			path: "link",
			modify: func(t *testing.T, a, b string) {
				t.Helper()

				for _, root := range []string{a, b} {
					if err := os.Symlink("target", filepath.Join(root, "link")); err != nil {
						t.Fatal(err)
					}
				}
			},
			want: true,
		},
		{
			name: "different symlinks",
			path: "link",
			modify: func(t *testing.T, a, b string) {
				t.Helper()

				if err := os.Symlink("target-a", filepath.Join(a, "link")); err != nil {
					t.Fatal(err)
				}

				if err := os.Symlink("target-b", filepath.Join(b, "link")); err != nil {
					t.Fatal(err)
				}
			},
			want: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := t.TempDir()
			b := t.TempDir()

			writeTestTree(t, a, tc.a)
			writeTestTree(t, b, tc.b)

			if tc.modify != nil {
				tc.modify(t, a, b)
			}

			got, err := ContentEqual(filepath.Join(a, tc.path), filepath.Join(b, tc.path))
			if err != nil {
				t.Fatalf("ContentEqual() error = %v", err)
			}

			if got != tc.want {
				t.Fatalf("ContentEqual() = %v, want %v", got, tc.want)
			}

			reversed, err := ContentEqual(filepath.Join(b, tc.path), filepath.Join(a, tc.path))
			if err != nil {
				t.Fatalf("ContentEqual() reversed error = %v", err)
			}

			if reversed != tc.want {
				t.Fatalf("ContentEqual() reversed = %v, want %v", reversed, tc.want)
			}
		})
	}
}
