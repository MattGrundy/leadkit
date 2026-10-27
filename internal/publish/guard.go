package publish

import (
	"fmt"
	"regexp"
	"strings"
)

// Runner is the seam every external command goes through (internal/forge.Runner satisfies it).
type Runner interface {
	Run(argv []string, stdin []byte) (stdout, stderr []byte, err error)
}

// Guard wraps a Runner so that the only commands this package can run are the ones it needs to
// PREPARE and VERIFY a tree: read the source repository, build a local one, and read a GitHub
// variable. Everything else is refused here, before it reaches the inner Runner.
//
// This is the code form of the standing order that no agent creates, pushes to, or otherwise
// changes a repository on a forge (tickets 198 and 267). It is an allow-list rather than a list
// of forbidden verbs because the second is a list of the ways someone has thought of, and it
// takes no flag, option or environment variable: nothing in this package can lift it.
func Guard(inner Runner) Runner { return guarded{inner} }

type guarded struct{ inner Runner }

func (g guarded) Run(argv []string, stdin []byte) ([]byte, []byte, error) {
	if err := Permitted(argv); err != nil {
		return nil, nil, err
	}
	return g.inner.Run(argv, stdin)
}

var (
	// gitConfigAllowed are the -c keys the tool sets itself. An alias or a transport setting
	// could turn an allowed subcommand into a disallowed one.
	gitConfigAllowed = map[string]bool{
		"user.name": true, "user.email": true, "commit.gpgsign": true,
		"core.hookspath": true, "core.autocrlf": true, "core.safecrlf": true, "core.excludesfile": true,
	}
	// gitSubcommands maps each permitted subcommand to the check on its remaining arguments.
	gitSubcommands = map[string]func(args []string) error{
		"archive": func(a []string) error {
			for _, x := range a {
				if strings.HasPrefix(x, "--remote") {
					return fmt.Errorf("archive --remote would contact another repository")
				}
			}
			return nil
		},
		"rev-parse": nil, "init": nil, "add": nil, "commit": nil,
		"log": nil, "rev-list": nil, "ls-files": nil, "status": nil,
		"reset": func(a []string) error {
			if len(a) == 3 && a[0] == "-q" && a[1] == "--hard" && sha40.MatchString(a[2]) {
				return nil
			}
			return fmt.Errorf("only `reset -q --hard <sha>` is permitted (the rollback of a commit this tool made)")
		},
		// Read-only, one fixed format and nothing else (no pattern, count or other flag): the refs
		// listing ScanRefs parses.
		"for-each-ref": func(a []string) error {
			if len(a) == 1 && a[0] == "--format="+refFormat {
				return nil
			}
			return fmt.Errorf("only `for-each-ref --format=<the fixed refs format>` is permitted")
		},
		// Read-only, and as narrow as the others: a tree id (never a name), recursive, NUL-separated;
		// and the batch reader, which takes object names on stdin and takes no other argument.
		"ls-tree": func(a []string) error {
			if len(a) == 3 && a[0] == "-r" && a[1] == "-z" && sha40.MatchString(a[2]) {
				return nil
			}
			return fmt.Errorf("only `ls-tree -r -z <tree-sha>` is permitted")
		},
		"cat-file": func(a []string) error {
			if len(a) == 1 && a[0] == "--batch" {
				return nil
			}
			return fmt.Errorf("only `cat-file --batch` is permitted")
		},
		"clean": func(a []string) error {
			if len(a) == 1 && a[0] == "-fdq" {
				return nil
			}
			return fmt.Errorf("only `clean -fdq` is permitted (it spares ignored files)")
		},
	}
	sha40 = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// Permitted reports whether argv is a command this package may run. It returns nil for a permitted
// command, and otherwise an error saying why not. Exported so a test can pin the allow-list
// without a Runner.
func Permitted(argv []string) error {
	if len(argv) == 0 {
		return refuse(argv, "empty command")
	}
	switch argv[0] {
	case "git":
		return permittedGit(argv)
	case "gh":
		// Reading one variable is the only thing gh is for here.
		if len(argv) >= 4 && argv[1] == "variable" && argv[2] == "get" {
			rest := argv[4:]
			if len(rest) == 0 || (len(rest) == 2 && rest[0] == "--repo") {
				return nil
			}
		}
		return refuse(argv, "gh may only run `variable get <name> [--repo <r>]`")
	}
	return refuse(argv, "only git and gh may be run")
}

func permittedGit(argv []string) error {
	i := 1
	for i < len(argv) {
		switch {
		case argv[i] == "-C" && i+1 < len(argv):
			i += 2
		case argv[i] == "-c" && i+1 < len(argv):
			key, _, _ := strings.Cut(argv[i+1], "=")
			if !gitConfigAllowed[strings.ToLower(key)] {
				return refuse(argv, "git -c "+key+" is not permitted")
			}
			i += 2
		default:
			goto sub
		}
	}
	return refuse(argv, "no git subcommand")
sub:
	name := argv[i]
	check, ok := gitSubcommands[name]
	if !ok {
		return refuse(argv, "git "+name+" is not on the allow-list")
	}
	if check != nil {
		if err := check(argv[i+1:]); err != nil {
			return refuse(argv, err.Error())
		}
	}
	return nil
}

func refuse(argv []string, why string) error {
	return fmt.Errorf("publish: refused %q: %s. This tool prepares and verifies a tree; it never creates, "+
		"pushes to or changes a repository on any forge, and no option lifts that (tickets 198, 267, 276)",
		strings.Join(argv, " "), why)
}
