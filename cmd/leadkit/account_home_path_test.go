package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The shell source for the account home is run by absolute path, so a program called `sh` or `id`
// earlier on PATH cannot answer for it. Each program is shimmed so that the shim's answer would
// show up among the sources if the program were found through PATH:
//
//   - a `sh` shim prints a forged home outright;
//   - an `id` shim prints a real account name other than the current one, whose home is derived
//     from the system. (An `id` shim that printed a forged PATH would assert nothing: the shell
//     expands `~/forged` against $HOME, so the result could never equal the forged value.)
func TestAccountHomes_AreNotAnsweredByAPathShim(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no shell source on windows")
	}
	for _, p := range []string{"/bin/sh", "/usr/bin/id"} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("no %s", p)
		}
	}
	// The home of another real account, asked of the system with PATH as it is before any shim.
	const other = "root"
	out, err := exec.Command("/bin/sh", "-c", "eval echo ~"+other).Output()
	otherHome := strings.TrimSpace(string(out))
	if err != nil || otherHome == "" || strings.HasPrefix(otherHome, "~") {
		t.Skipf("cannot resolve the home of %q on this machine", other)
	}
	for _, h := range accountHomes() {
		if h == otherHome {
			t.Skipf("this process runs as %q, so its own home is %s and a shim cannot be told from the real answer", other, otherHome)
		}
	}

	shims := t.TempDir()
	const forged = "/forged-home"
	for name, answer := range map[string]string{"sh": forged, "id": other} {
		body := "#!/bin/sh\necho " + answer + "\n"
		if err := os.WriteFile(filepath.Join(shims, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", shims+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, h := range accountHomes() {
		switch h {
		case forged:
			t.Errorf("an `sh` found through PATH answered for the account home: %v", accountHomes())
		case otherHome:
			t.Errorf("an `id` found through PATH named another account (%s) and its home %s became a source: %v", other, otherHome, accountHomes())
		}
	}
}
