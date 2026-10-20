// Package release tests install.sh against a file:// release directory.
package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// setup builds the binary into a release dir and returns the dir, a HOME and a PATH holding only the tools the
// script needs.
func setup(t *testing.T) (rel, home, path string) {
	t.Helper()
	root, _ := filepath.Abs("../..")
	rel, home = t.TempDir(), t.TempDir()
	name := "habla-linux-" + runtime.GOARCH
	build := exec.Command("go", "build", "-ldflags", "-X main.version=9.9.9", "-o", filepath.Join(rel, name), ".")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOTOOLCHAIN=local")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	sum := exec.Command("sha256sum", name)
	sum.Dir = rel
	out, err := sum.Output()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rel, "SHA256SUMS"), out, 0o644); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	for _, tool := range strings.Fields("sh curl sha256sum uname chmod mkdir mv grep head cat mktemp rm") {
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s not available", tool)
		}
		if err := os.Symlink(p, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	return rel, home, bin
}

func run(t *testing.T, rel, home, path string, extra ...string) (string, error) {
	t.Helper()
	root, _ := filepath.Abs("../..")
	cmd := exec.Command(filepath.Join(path, "sh"), filepath.Join(root, "install.sh"))
	cmd.Env = []string{
		"PATH=" + path, "HOME=" + home, "HABLA_RELEASE_URL=file://" + rel,
		"XDG_CONFIG_HOME=" + home + "/.config", "XDG_DATA_HOME=" + home + "/.local/share",
		"XDG_STATE_HOME=" + home + "/.local/state", "XDG_RUNTIME_DIR=" + home + "/run",
	}
	cmd.Env = append(cmd.Env, extra...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestInstall(t *testing.T) {
	rel, home, path := setup(t)
	out, err := run(t, rel, home, path)
	if err != nil {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}
	bin := filepath.Join(home, ".local/bin/habla")
	if fi, err := os.Stat(bin); err != nil || fi.Mode()&0o111 == 0 {
		t.Fatalf("not installed or not executable: %v\n%s", err, out)
	}
	if v, err := exec.Command(bin, "--version").Output(); err != nil || string(v) != "habla 9.9.9\n" {
		t.Fatalf("--version = %q, %v", v, err)
	}
	if _, err := os.Stat(bin + ".new"); err == nil {
		t.Fatal("habla.new left behind")
	}
	// Re-running upgrades in place.
	if out, err := run(t, rel, home, path); err != nil {
		t.Fatalf("second run: %v\n%s", err, out)
	}
}

func TestBadChecksumInstallsNothing(t *testing.T) {
	rel, home, path := setup(t)
	bad := strings.Repeat("0", 64) + "  habla-linux-" + runtime.GOARCH + "\n"
	if err := os.WriteFile(filepath.Join(rel, "SHA256SUMS"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, rel, home, path)
	if err == nil || !strings.Contains(out, "checksum mismatch") {
		t.Fatalf("want a checksum failure, got %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(home, ".local/bin/habla")); err == nil {
		t.Fatal("binary installed despite the bad checksum")
	}
}

func TestInstallDir(t *testing.T) {
	rel, home, path := setup(t)
	dir := filepath.Join(home, "custom")
	if out, err := run(t, rel, home, path, "HABLA_INSTALL_DIR="+dir); err != nil {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "habla")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".local/bin/habla")); err == nil {
		t.Fatal("installed in the default dir too")
	}
}
