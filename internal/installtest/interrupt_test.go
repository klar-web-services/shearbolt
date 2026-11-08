//go:build unix

package installtest

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"
)

// An interrupt while the archive downloads stops the installer: it exits non-zero, TMPDIR is
// cleaned up and nothing is installed. "shell only" signals just the shell while curl keeps
// going and then completes the download, so the trap, not a failed fetch, has to stop the run.
func TestInstallInterruptLeavesNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		sig   syscall.Signal
		group bool
	}{
		{"INT to the process group", syscall.SIGINT, true},
		{"TERM to the process group", syscall.SIGTERM, true},
		{"HUP to the process group", syscall.SIGHUP, true},
		{"TERM to the shell only", syscall.SIGTERM, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := http.FileServer(http.Dir(fakeRelease(t, "1.2.3", linuxAmd64)))
			hit := make(chan struct{}, 1)
			release := make(chan struct{})
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, ".tar.gz") {
					select {
					case hit <- struct{}{}:
					default:
					}
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
				}
				files.ServeHTTP(w, r)
			}))
			t.Cleanup(ts.Close)
			t.Cleanup(func() { close(release) }) // runs first, so ts.Close does not wait on a held request

			dir := installDir(t)
			p := prepare(t, invocation{
				env:   map[string]string{"SHEARBOLT_BASE_URL": ts.URL, "SHEARBOLT_INSTALL_DIR": dir},
				tools: tools("curl"),
			})
			p.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := p.cmd.Start(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-hit:
			case <-time.After(10 * time.Second):
				_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
				t.Fatalf("no archive request\nstderr:\n%s", p.stderr.String())
			}
			pid := p.cmd.Process.Pid
			if tc.group {
				pid = -pid
			}
			if err := syscall.Kill(pid, tc.sig); err != nil {
				t.Fatal(err)
			}
			if !tc.group {
				time.Sleep(300 * time.Millisecond)
				select {
				case release <- struct{}{}: // curl completes the download
				case <-time.After(5 * time.Second):
					t.Fatal("the held archive request went away")
				}
			}
			r := p.finish(t, p.cmd.Wait())
			if r.code == 0 {
				t.Fatalf("exit 0 after %v\nstderr:\n%s", tc.sig, r.stderr)
			}
			if strings.Contains(r.stdout+r.stderr, "Installed shearbolt") {
				t.Fatalf("interrupted run claims an install:\n%s%s", r.stdout, r.stderr)
			}
			wantUntouched(t, dir, nil)
		})
	}
}
