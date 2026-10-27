package main

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
)

// trustHomeEnv is "1" only in a binary built by the test suite (-ldflags -X), so that the
// subprocess tests, whose HOME is a temp directory and never the account's, can reach the
// unconfigured state at all. It is a link-time constant, not a switch an environment variable or
// a command line can flip: a binary built without the flag always asks the account.
var trustHomeEnv string

// accountHomes returns the home directory of the account running this process, from sources
// that do not read $HOME. There are two because each can fail alone: os/user falls back to $HOME
// when it cannot find the account (for example a uid that is absent from /etc/passwd in a
// pure-Go build), and then it only agrees with whatever HOME says; the shell's `~name` asks the
// system for the account `id -un` names. Both programs are run by absolute path, so a PATH shim
// cannot answer for them. An injectable var so a test can say what the account is.
var accountHomes = func() []string {
	var homes []string
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		homes = append(homes, u.HomeDir)
	}
	if runtime.GOOS != "windows" {
		out, err := exec.Command("/bin/sh", "-c", `eval echo ~"$(/usr/bin/id -un)"`).Output()
		if h := strings.TrimSpace(string(out)); err == nil && h != "" && !strings.HasPrefix(h, "~") {
			homes = append(homes, h)
		}
	}
	return homes
}

// homeIsTheAccountsOwn reports whether $HOME is the home directory of the account running this
// process. A HOME that is not (`HOME=/tmp/x leadkit push`) names a place where leadkit's
// external configs are not, so a repo's missing profile cannot be told from one that is merely
// not there, and the unconfigured state must not be reached through it. With no source to
// ask, or a source that disagrees, it is not.
func homeIsTheAccountsOwn() bool {
	if trustHomeEnv == "1" {
		return true
	}
	home := os.Getenv("HOME")
	if home == "" {
		return false
	}
	homes := accountHomes()
	if len(homes) == 0 {
		return false
	}
	same := func(a, b string) bool {
		if filepath.Clean(a) == filepath.Clean(b) {
			return true
		}
		ra, errA := filepath.EvalSymlinks(a)
		rb, errB := filepath.EvalSymlinks(b)
		return errA == nil && errB == nil && ra == rb
	}
	for _, h := range homes {
		if !same(home, h) {
			return false
		}
	}
	return true
}
