package test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// StartGitDaemon serves only repositories below the generated test root over
// loopback TCP. It returns a git://127.0.0.1:<port>/ base URL and stops the daemon
// and its children when the test ends. Missing Git executables skip the test.
func StartGitDaemon(t *testing.T, root string) string {
	t.Helper()

	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("native Git integration tests require the git executable: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	execPath, err := exec.CommandContext(ctx, gitPath, "--exec-path").Output()

	cancel()

	if err != nil {
		t.Fatalf("locate git daemon: %v", err)
	}

	if _, err := exec.LookPath(filepath.Join(strings.TrimSpace(string(execPath)), "git-daemon")); err != nil {
		t.Skipf("native Git integration tests require the git daemon executable: %v", err)
	}

	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatalf("resolve Git daemon root: %v", err)
	}

	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve Git daemon root symlinks: %v", err)
	}

	probePath, err := os.MkdirTemp(root, ".git-daemon-ready-")
	if err != nil {
		t.Fatalf("create Git daemon readiness repository: %v", err)
	}

	t.Cleanup(func() {
		if err := os.RemoveAll(probePath); err != nil {
			t.Errorf("remove Git daemon readiness repository: %v", err)
		}
	})

	probe, err := gogit.PlainInit(probePath, true)
	if err != nil {
		t.Fatalf("initialize Git daemon readiness repository: %v", err)
	}

	raw := &plumbing.MemoryObject{}
	raw.SetType(plumbing.BlobObject)

	writer, err := raw.Writer()
	if err != nil {
		t.Fatalf("create readiness object writer: %v", err)
	}

	if _, err := writer.Write([]byte(probePath)); err != nil {
		t.Fatalf("write readiness object: %v", err)
	}

	if err := writer.Close(); err != nil {
		t.Fatalf("close readiness object writer: %v", err)
	}

	hash, err := probe.Storer.SetEncodedObject(raw)
	if err != nil {
		t.Fatalf("store readiness object: %v", err)
	}

	probeRef := plumbing.NewTagReferenceName("ready")
	if err := probe.Storer.SetReference(plumbing.NewHashReference(probeRef, hash)); err != nil {
		t.Fatalf("store readiness reference: %v", err)
	}

	var failures strings.Builder

	for attempt := range 3 {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("select Git daemon port: %v", err)
		}

		port := listener.Addr().(*net.TCPAddr).Port
		if err := listener.Close(); err != nil {
			t.Fatalf("release Git daemon port: %v", err)
		}

		baseURL := "git://127.0.0.1:" + strconv.Itoa(port) + "/"
		output := &gitDaemonOutput{}
		cmd := exec.Command(gitPath, "daemon",
			"--reuseaddr",
			"--base-path="+root,
			"--listen=127.0.0.1",
			"--port="+strconv.Itoa(port),
			"--export-all",
			"--verbose",
			"--init-timeout=2",
			"--timeout=5",
			root,
		)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Stdout = output
		cmd.Stderr = output

		if err := cmd.Start(); err != nil {
			t.Fatalf("start Git daemon: %v\nstdout/stderr:\n%s", err, output.String())
		}

		done := make(chan struct{})

		var waitErr error

		go func() {
			waitErr = cmd.Wait()

			close(done)
		}()

		stop := func() {
			// The new process group contains only this daemon and its upload-pack
			// children, including children still running after the daemon exits.
			if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				t.Errorf("stop Git daemon process group: %v", err)
			}

			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Errorf("Git daemon did not stop\nstdout/stderr:\n%s", output.String())
			}
		}

		probeURL := baseURL + filepath.Base(probePath)
		want := hash.String() + "\t" + probeRef.String()
		deadline := time.Now().Add(3 * time.Second)

		var probeErr error

		for time.Now().Before(deadline) {
			select {
			case <-done:
				probeErr = fmt.Errorf("daemon exited: %v", waitErr)
			default:
				probeCtx, probeCancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
				probeCmd := exec.CommandContext(probeCtx, gitPath, "ls-remote", "--exit-code", probeURL, probeRef.String())

				probeCmd.Env = append(os.Environ(),
					"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
					"GIT_TERMINAL_PROMPT=0", "GIT_PROTOCOL=version=0",
				)
				probeOutput, err := probeCmd.CombinedOutput()

				probeCancel()

				if err == nil && strings.TrimSpace(string(probeOutput)) == want {
					select {
					case <-done:
						probeErr = fmt.Errorf("daemon exited after readiness request: %v", waitErr)
					default:
						t.Cleanup(func() {
							stop()

							if t.Failed() {
								t.Logf("Git daemon stdout/stderr:\n%s", output.String())
							}
						})

						return baseURL
					}
				} else {
					probeErr = fmt.Errorf("readiness request: %v, output: %q", err, probeOutput)
				}
			}

			select {
			case <-done:
				deadline = time.Now()
			case <-time.After(25 * time.Millisecond):
			}
		}

		stop()
		fmt.Fprintf(&failures, "attempt %d, port %d: %v\nstdout/stderr:\n%s\n", attempt+1, port, probeErr, output.String())
	}

	t.Fatalf("Git daemon failed to serve its readiness repository after bounded retries:\n%s", failures.String())

	return ""
}

type gitDaemonOutput struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (o *gitDaemonOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.buffer.Write(p)
}

func (o *gitDaemonOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.buffer.String()
}
