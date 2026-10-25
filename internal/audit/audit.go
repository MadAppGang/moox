// Package audit holds the static analyses that turn four of this feature's
// rules into tests rather than review conventions.
//
//	§9.4a  code we ship never writes to ~/.claude/ — keyed on PACKAGE, because
//	       that is what an AST pass can actually decide (a runtime path cannot
//	       be evaluated by go/ast, and the path is a constructor parameter
//	       precisely so it can be t.TempDir()).
//	§9.4a  every exec.Command("tmux", …) in a TEST file carries both
//	       -f /dev/null and -L. Revision 0 claimed this was covered by the pass
//	       above, which walks non-test files, so as written it audited nothing.
//	§9.3a  the timer rule, plus the T5 spawn precondition.
//	§5.1   omitempty is banned, and json tags exist only in the wire package.
//
// # Why the scanners are exported and the tests are thin
//
// An audit that can only be run over the real tree cannot be shown to work. A
// scanner that finds nothing is indistinguishable from a scanner that looks at
// nothing, and a rule nobody can see fail is not enforced — it is decoration.
// So every scanner here is a pure function over parsed files, which lets each
// audit test assert two things: zero findings over the real module, and at
// least one finding over a deliberately violating snippet. The second
// assertion is what makes the first one mean something.
package audit

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
)

// Finding is one rule violation, located precisely enough to fix.
type Finding struct {
	// File is the path relative to the module root.
	File string
	Line int
	// Func is the enclosing function name, or "" at package scope.
	Func string
	// Rule names which audit produced this.
	Rule string
	// Detail is what was found.
	Detail string
}

func (f Finding) String() string {
	where := f.File + ":" + strconv.Itoa(f.Line)
	if f.Func != "" {
		where += " (in " + f.Func + ")"
	}
	return fmt.Sprintf("%s: %s: %s", where, f.Rule, f.Detail)
}

// Format renders findings one per line, for a test failure message.
func Format(findings []Finding) string {
	var b strings.Builder
	for _, f := range findings {
		b.WriteString("  ")
		b.WriteString(f.String())
		b.WriteByte('\n')
	}
	return b.String()
}

// File is one parsed Go source file.
type File struct {
	// Rel is the slash-separated path relative to the module root.
	Rel string
	// Dir is Rel's directory, which is the package key every package-scoped
	// rule is decided on.
	Dir    string
	IsTest bool
	AST    *ast.File
	Fset   *token.FileSet
}

// ModulePath is the module this audit polices. The audit test checks the
// module root's go.mod declares it (moox architecture §2.2 step 4).
const ModulePath = "github.com/MadAppGang/moox"

// ModuleRoot walks up from start looking for go.mod. Tests run with the
// package directory as their working directory, and this module's root is
// moox/ — tt's main package sits two levels below it in cmd/tt, which is
// exactly the sort of thing a hardcoded "../.." gets wrong when a package moves.
func ModuleRoot(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("audit: no go.mod found above " + start)
		}
		dir = parent
	}
}

// LoadModule parses every .go file in the module, test files included, and
// tags each with whether it is a test. Directories the Go tool itself ignores
// — testdata and vendor — are skipped, along with anything starting with a dot
// or underscore, third_party/, and any directory holding its own go.mod: a
// nested module, which `go test ./...` skips too (moox §2.4, R2-H11: the
// vendored rpaster source has its own module and its own rules).
func LoadModule(root string) ([]File, error) {
	var out []File
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if p == root {
				return nil
			}
			if name == "testdata" || name == "vendor" || name == "third_party" ||
				strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				return fs.SkipDir
			}
			if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") {
			return nil
		}

		src, err := parser.ParseFile(fset, p, nil, parser.ParseComments)
		if err != nil {
			return fmt.Errorf("audit: parsing %s: %w", p, err)
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		out = append(out, File{
			Rel:    rel,
			Dir:    path.Dir(rel),
			IsTest: strings.HasSuffix(name, "_test.go"),
			AST:    src,
			Fset:   fset,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// LoadSource parses one in-memory source file under the given relative path.
// This is what lets an audit test prove its scanner can fail.
func LoadSource(rel, src string) (File, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, src, parser.ParseComments)
	if err != nil {
		return File{}, err
	}
	rel = filepath.ToSlash(rel)
	return File{
		Rel:    rel,
		Dir:    path.Dir(rel),
		IsTest: strings.HasSuffix(rel, "_test.go"),
		AST:    f,
		Fset:   fset,
	}, nil
}

// NonTest and Tests split a file set.
func NonTest(files []File) []File { return filterFiles(files, false) }

// Tests returns only the _test.go files.
func Tests(files []File) []File { return filterFiles(files, true) }

func filterFiles(files []File, wantTest bool) []File {
	out := make([]File, 0, len(files))
	for _, f := range files {
		if f.IsTest == wantTest {
			out = append(out, f)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// §9.4a (a) — the package-keyed write audit
// ---------------------------------------------------------------------------

// WriteCalls are the os-package calls that can modify the filesystem.
// os.OpenFile, unix.Open and syscall.Open are handled separately, by their
// flags.
var WriteCalls = []string{
	"Create", "CreateTemp", "WriteFile", "Remove", "RemoveAll", "Rename",
	"Mkdir", "MkdirAll", "MkdirTemp", "Chmod", "Chown", "Chtimes", "Symlink",
	"Link", "Truncate",
}

// SyscallWriteCalls are the POSIX spellings of the same thing, for the `unix`
// and `syscall` qualifiers.
//
// This list is not an embellishment. AC13's claim is that "nothing we ship
// writes under ~/.claude/" is STATICALLY audited — true rather than merely
// currently-true — and the audit was keyed on os-package names alone, so ten
// mutating calls placed in a non-allowlisted package produced ZERO findings
// while an os-package control produced three of three. The gap landed exactly
// where it mattered: watchkq is not allowlisted, imports golang.org/x/sys/unix,
// and holds ~57 live descriptors on paths inside ~/.claude/. The descriptors
// are opened O_RDONLY|O_EVTONLY so a write THROUGH one fails with EBADF, but
// the path-taking calls — Unlink, Rmdir, Renameat — have no such mitigation.
//
// Kept separate from WriteCalls rather than merged into it so that each list
// says which surface it polices, and so a reviewer can see at a glance that
// neither one silently covers the other.
var SyscallWriteCalls = []string{
	"Unlink", "Unlinkat", "Rmdir", "Mkdir", "Mkdirat", "Mknod", "Mknodat",
	"Write", "Writev", "Pwrite", "Pwritev", "Ftruncate", "Truncate",
	"Fchmod", "Fchmodat", "Chmod", "Fchown", "Fchownat", "Chown", "Lchown",
	"Rename", "Renameat", "Renameatx_np", "Renamexat_np", "RenameatxNp", "RenamexNp", "Renameat2",
	"Symlink", "Symlinkat", "Link", "Linkat",
	"Setxattr", "Fsetxattr", "Removexattr", "Fremovexattr",
	"Utimes", "UtimesNano", "Futimes", "Futimesat",
	"Creat", "Mkfifo", "Mkfifoat", "Clonefile", "Clonefileat", "Fclonefileat",
	"Exchangedata", "Undelete", "Copyfile", "Fcopyfile",
}

// writeCallsFor is the banned-name list for one package qualifier. `unix` and
// `syscall` get BOTH lists: the POSIX one, and the os-spelled one because a
// name like Truncate or Chmod is spelled the same in either and a reader
// should not have to know which list caught it.
func writeCallsFor(pkg string) [][]string {
	switch pkg {
	case "unix", "syscall":
		return [][]string{WriteCalls, SyscallWriteCalls}
	default:
		return [][]string{WriteCalls}
	}
}

// OpenWithFlags are the openers whose flag argument decides whether they
// write, keyed by the fully qualified name because the qualifier is what
// settles it: os.Open takes no flags and is read-only by definition, while
// unix.Open's second argument is the flag set. Keying on the bare method name
// would reject every os.Open in the module — which it did, on the first run of
// this audit, against restore.go's snapshot reader.
var OpenWithFlags = map[string]int{
	// qualified name -> index of the flag argument
	"os.OpenFile":  1, // os.OpenFile(name, flag, perm)
	"unix.Open":    1, // unix.Open(path, mode, perm)
	"unix.Openat":  2, // unix.Openat(dirfd, path, mode, perm)
	"syscall.Open": 1, // syscall.Open(path, mode, perm)
}

// pureReadFlags is the only flag combination a read-only opener may use. §9.4a
// names the first four; O_EVTONLY is the darwin flag the kqueue watcher needs,
// and it deliberately does not touch atime. O_DIRECTORY and O_NOFOLLOW were
// added with statedir (code review round 2, A2): both only NARROW what an open
// may reach — a directory, and not through a symlink — and neither can make an
// open writable.
var pureReadFlags = map[string]bool{
	"O_RDONLY":    true,
	"O_EVTONLY":   true,
	"O_CLOEXEC":   true,
	"O_NONBLOCK":  true,
	"O_DIRECTORY": true,
	"O_NOFOLLOW":  true,
}

// fsPackages are the package qualifiers whose write calls this audit polices.
var fsPackages = map[string]bool{"os": true, "unix": true, "syscall": true, "ioutil": true}

// BindCalls are the `net` calls that create a FILE: binding a unix socket puts
// an inode on disk at a path, which is a write in every sense this audit cares
// about, and the socket server's bind used to sit outside any check at all.
var BindCalls = []string{"Listen", "ListenUnix", "ListenUnixgram", "ListenPacket"}

// WritingPackages is the enumerated exception list: the ONE package in the
// module allowed to write at all.
//
// It used to name two — sockserve (socket + pid file) and agentscli (cache +
// lock) — and exempt both WHOLESALE, which is how code review round 2's A2
// slipped through: both checked containment by path spelling, a state
// directory reached through a symlink into ~/.claude passed, and os.WriteFile
// followed a symlinked observer.pid. Being exempt from the audit said nothing
// about HOW they wrote. Both now write through statedir, and statedir is held
// to a stricter rule than the exemption ever was (ContainedWrites): inside it,
// every mutation must be relative to the verified directory descriptor, and
// every writing open must refuse to follow a symlink.
//
// The list is enumerated rather than inferred, so adding a write anywhere else
// fails this audit, and adding a second writing package requires editing this
// line deliberately.
var WritingPackages = []string{
	"internal/statedir", // every write tt makes, fd-relative (§9.4a, A2); moved from internal/observer/adapters/statedir (moox §2.4)
}

// PathWriteCalls are the POSIX mutators that take a PATH rather than a
// directory descriptor. Inside the writing package they are findings even
// though the package may write: a path-taking call resolves every component
// again, so it can follow a symlink the directory check never saw.
var PathWriteCalls = []string{
	"Unlink", "Rmdir", "Mkdir", "Mknod", "Truncate", "Chmod", "Chown", "Lchown",
	"Rename", "Symlink", "Link", "Setxattr", "Removexattr", "Utimes", "UtimesNano",
	"Lutimes", "Creat", "Mkfifo", "Clonefile", "Exchangedata", "Undelete", "Copyfile",
}

// dirRelativeWriteCalls are the *at mutators the writing package is meant to
// use. Their first argument is the directory descriptor, and AT_FDCWD there
// would make one path-relative again, so that spelling is a finding too.
var dirRelativeWriteCalls = map[string]bool{
	"Unlinkat": true, "Mkdirat": true, "Renameat": true, "Fchmodat": true,
	"Fchownat": true, "Linkat": true, "Symlinkat": true, "Mknodat": true, "Mkfifoat": true,
	// the no-replace renames statedir.RenameAt uses (darwin, linux); both
	// take a source AND a destination descriptor, checked as Renameat's are
	"RenameatxNp": true, "Renameat2": true,
}

// Writes reports every filesystem-mutating call outside the allowed packages.
//
// It flags REFERENCES, not only calls: `var w = os.WriteFile` is a call site
// one indirection away, and an audit that only looked at CallExpr would be
// defeated by a variable. The same walk therefore approves os.OpenFile at the
// positions where it is called with a pure-read flag set, and reports every
// other appearance of it.
func Writes(files []File, allowedDirs []string) []Finding {
	allowed := make(map[string]bool, len(allowedDirs))
	for _, d := range allowedDirs {
		allowed[d] = true
	}

	var findings []Finding
	for _, f := range files {
		if allowed[f.Dir] {
			continue
		}
		approved := approvedReadOpens(f)

		walkWithFuncs(f, func(fnName string, n ast.Node) {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return
			}
			name := sel.Sel.Name
			if pkg.Name == "net" {
				for _, bind := range BindCalls {
					if name == bind {
						findings = append(findings, Finding{
							File: f.Rel, Line: line(f, sel.Pos()), Func: fnName,
							Rule:   "write-audit",
							Detail: fmt.Sprintf("net.%s binds a socket, which creates a file", name),
						})
						return
					}
				}
				return
			}
			if !fsPackages[pkg.Name] {
				return
			}

			if _, isOpener := OpenWithFlags[pkg.Name+"."+name]; isOpener {
				if approved[sel.Pos()] {
					return
				}
				findings = append(findings, Finding{
					File: f.Rel, Line: line(f, sel.Pos()), Func: fnName,
					Rule: "write-audit",
					Detail: fmt.Sprintf("%s.%s without a provably read-only flag set (allowed: %s)",
						pkg.Name, name, strings.Join(sortedKeys(pureReadFlags), "|")),
				})
				return
			}
			for _, list := range writeCallsFor(pkg.Name) {
				for _, banned := range list {
					if name == banned {
						findings = append(findings, Finding{
							File: f.Rel, Line: line(f, sel.Pos()), Func: fnName,
							Rule:   "write-audit",
							Detail: fmt.Sprintf("%s.%s writes to the filesystem", pkg.Name, name),
						})
						return
					}
				}
			}
		})
	}
	return findings
}

// approvedReadOpens finds the positions of opener selectors that are being
// CALLED with a flag argument built only from pure-read flags.
func approvedReadOpens(f File) map[token.Pos]bool {
	approved := map[token.Pos]bool{}
	ast.Inspect(f.AST, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || !fsPackages[pkg.Name] {
			return true
		}
		idx, isOpener := OpenWithFlags[pkg.Name+"."+sel.Sel.Name]
		if !isOpener || len(call.Args) <= idx {
			return true
		}
		if isPureReadFlagExpr(call.Args[idx]) {
			approved[sel.Pos()] = true
		}
		return true
	})
	return approved
}

// ContainedWrites holds the writing package itself to the rule its exemption
// exists for: every mutation it makes is RELATIVE to a verified directory
// descriptor and none of them follows a symlink.
//
// Concretely, inside a writing package:
//
//	os.* / ioutil.* write calls          finding — every one of them takes a path
//	unix/syscall path-taking mutators    finding — PathWriteCalls
//	os.OpenFile / unix.Open with flags   finding unless the flags are pure-read
//	unix.Openat with write flags         finding unless the flags include O_NOFOLLOW
//	*at mutators given AT_FDCWD          finding — that is a path call in disguise
//
// This is what the old wholesale exemption never checked, and it is the gap
// A2 went through: the exempt packages wrote with os.WriteFile and os.Rename
// after a lexical containment check.
func ContainedWrites(files []File, writingDirs []string) []Finding {
	writing := make(map[string]bool, len(writingDirs))
	for _, d := range writingDirs {
		writing[d] = true
	}
	var findings []Finding
	for _, f := range files {
		if !writing[f.Dir] {
			continue
		}
		report := func(fnName string, pos token.Pos, detail string) {
			findings = append(findings, Finding{
				File: f.Rel, Line: line(f, pos), Func: fnName, Rule: "contained-write", Detail: detail,
			})
		}
		calls := map[*ast.SelectorExpr]*ast.CallExpr{}
		ast.Inspect(f.AST, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					calls[sel] = call
				}
			}
			return true
		})

		walkWithFuncs(f, func(fnName string, n ast.Node) {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || !fsPackages[pkg.Name] {
				return
			}
			name := sel.Sel.Name
			call := calls[sel]

			switch {
			case pkg.Name == "os" || pkg.Name == "ioutil":
				if name == "OpenFile" {
					if call == nil || len(call.Args) < 2 || !isPureReadFlagExpr(call.Args[1]) {
						report(fnName, sel.Pos(), "os.OpenFile with write flags takes a path; use unix.Openat on the verified directory")
					}
					return
				}
				for _, banned := range WriteCalls {
					if name == banned {
						report(fnName, sel.Pos(), fmt.Sprintf("%s.%s takes a path; write relative to the verified directory descriptor instead", pkg.Name, name))
						return
					}
				}
			case name == "Open":
				if call == nil || len(call.Args) < 2 || !isPureReadFlagExpr(call.Args[1]) {
					report(fnName, sel.Pos(), fmt.Sprintf("%s.Open with write flags takes a path; use Openat on the verified directory", pkg.Name))
				}
			case name == "Openat":
				if call == nil || len(call.Args) < 3 {
					report(fnName, sel.Pos(), fmt.Sprintf("%s.Openat used as a value; its flags cannot be checked", pkg.Name))
					return
				}
				if isAtFDCWD(call.Args[0]) {
					report(fnName, sel.Pos(), fmt.Sprintf("%s.Openat with AT_FDCWD is a path-relative open", pkg.Name))
					return
				}
				if !isPureReadFlagExpr(call.Args[2]) && !flagExprHas(call.Args[2], "O_NOFOLLOW") {
					report(fnName, sel.Pos(), fmt.Sprintf("%s.Openat with write flags must include O_NOFOLLOW", pkg.Name))
				}
			case dirRelativeWriteCalls[name]:
				if call == nil || len(call.Args) == 0 || isAtFDCWD(call.Args[0]) {
					report(fnName, sel.Pos(), fmt.Sprintf("%s.%s must be given the verified directory descriptor, not AT_FDCWD", pkg.Name, name))
				}
				if name == "Renameat" && call != nil && len(call.Args) >= 3 && isAtFDCWD(call.Args[2]) {
					report(fnName, sel.Pos(), fmt.Sprintf("%s.Renameat's destination must be the verified directory descriptor, not AT_FDCWD", pkg.Name))
				}
			default:
				for _, banned := range PathWriteCalls {
					if name == banned {
						report(fnName, sel.Pos(), fmt.Sprintf("%s.%s takes a path; use the *at form on the verified directory descriptor", pkg.Name, name))
						return
					}
				}
			}
		})
	}
	return findings
}

// isAtFDCWD reports whether an expression spells AT_FDCWD.
func isAtFDCWD(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.SelectorExpr:
		return v.Sel.Name == "AT_FDCWD"
	case *ast.Ident:
		return v.Name == "AT_FDCWD"
	case *ast.ParenExpr:
		return isAtFDCWD(v.X)
	default:
		return false
	}
}

// flagExprHas reports whether an OR-expression of flag constants includes the
// named flag.
func flagExprHas(e ast.Expr, flag string) bool {
	switch v := e.(type) {
	case *ast.BinaryExpr:
		return v.Op == token.OR && (flagExprHas(v.X, flag) || flagExprHas(v.Y, flag))
	case *ast.ParenExpr:
		return flagExprHas(v.X, flag)
	case *ast.SelectorExpr:
		return v.Sel.Name == flag
	case *ast.Ident:
		return v.Name == flag
	default:
		return false
	}
}

// isPureReadFlagExpr reports whether an expression is an OR of pure-read flag
// constants, or a literal 0 (which is O_RDONLY).
func isPureReadFlagExpr(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.BinaryExpr:
		return v.Op == token.OR && isPureReadFlagExpr(v.X) && isPureReadFlagExpr(v.Y)
	case *ast.ParenExpr:
		return isPureReadFlagExpr(v.X)
	case *ast.SelectorExpr:
		return pureReadFlags[v.Sel.Name]
	case *ast.Ident:
		return pureReadFlags[v.Name]
	case *ast.BasicLit:
		return v.Kind == token.INT && v.Value == "0"
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// §9.4a (b) — the test-file tmux pass
// ---------------------------------------------------------------------------

// TmuxExecInTests reports every exec.Command("tmux", …) in a test file that
// does not provably target an isolated server.
//
// Both flags are required, and for different reasons:
//
//	-f /dev/null  the server must not source ~/.tmux.conf. On 2026-08-17 a
//	              scratch server did, and its plugins cloned the whole
//	              workspace: 16 sessions, 44 windows, 71 panes destroyed.
//	-L <name>     a private socket. Without it "isolated" means "the user's
//	              live server", and the tests run on the machine that carries
//	              the user's actual work.
//
// Flags must be string LITERALS. A flag assembled at runtime cannot be checked
// here, and "we could not tell" has to fail: the cost of a false positive is
// one explicit test-only exemption, the cost of a false negative is the
// workspace.
func TmuxExecInTests(files []File) []Finding {
	var findings []Finding
	for _, f := range files {
		if !f.IsTest {
			continue
		}
		walkWithFuncs(f, func(fnName string, n ast.Node) {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "exec" {
				return
			}
			if sel.Sel.Name != "Command" && sel.Sel.Name != "CommandContext" {
				return
			}

			lits := stringLiterals(call.Args)
			if !containsString(lits, "tmux") {
				return
			}
			missing := []string{}
			if !containsString(lits, "-f") || !containsString(lits, "/dev/null") {
				missing = append(missing, "-f /dev/null")
			}
			if !containsString(lits, "-L") {
				missing = append(missing, "-L <private socket>")
			}
			if len(missing) > 0 {
				findings = append(findings, Finding{
					File: f.Rel, Line: line(f, call.Pos()), Func: fnName,
					Rule:   "test-tmux-isolation",
					Detail: "tmux exec without " + strings.Join(missing, " and "),
				})
			}
		})
	}
	return findings
}

// ---------------------------------------------------------------------------
// §9.3a — the timer rule and the T5 spawn precondition
// ---------------------------------------------------------------------------

// TimerCalls are the four time-package entry points §9.3a bans outside the
// allowlist. Ticker and Tick are banned because they re-arm themselves; After
// and Sleep are banned because they are the two easiest ways to arm something
// on a path nobody is looking at.
//
// time.NewTimer is deliberately NOT here: a one-shot timer armed in response
// to an event and cancelled on success is precisely what the timer rule
// permits, and the four clauses are about arming discipline, not about the
// constructor used.
var TimerCalls = []string{"Tick", "NewTicker", "After", "Sleep"}

// TimerAllow is one allowlist entry. §9.3a fixes the format as
// (site, rule-citation): a site with no citation is an exemption nobody
// argued for, and the citation is what a later reader checks the entry
// against.
type TimerAllow struct {
	// Site is "<file relative to module root>:<enclosing func>", with an empty
	// func meaning package scope.
	Site string
	// Citation is the section of the architecture that permits it.
	Citation string
}

// TimerAllowlist is the whole of it. Every entry must satisfy all four clauses
// of the timer rule: one-shot, armed only by an event or once at start,
// self-disarming, and never re-armed while idle.
var TimerAllowlist = []TimerAllow{
	{
		Site: "internal/observer/adapters/registryfs/registryfs.go:newSource",
		Citation: "§6.5 torn-read retry (10ms, <=4 per rebuild): armed by a parse " +
			"failure, fires once per retried file, never armed while idle. " +
			"§9.3a names it as time.Sleep's one exception.",
	},
}

// Timers reports every banned time-package reference outside the allowlist.
func Timers(files []File, allow []TimerAllow) []Finding {
	allowed := make(map[string]bool, len(allow))
	for _, a := range allow {
		if strings.TrimSpace(a.Citation) == "" {
			// An entry without a citation does not grant an exemption. Stated
			// as behaviour rather than as a comment, because the format rule is
			// the only thing stopping the allowlist becoming a junk drawer.
			continue
		}
		allowed[a.Site] = true
	}

	var findings []Finding
	for _, f := range files {
		walkWithFuncs(f, func(fnName string, n ast.Node) {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "time" {
				return
			}
			if !containsString(TimerCalls, sel.Sel.Name) {
				return
			}
			if allowed[f.Rel+":"+fnName] {
				return
			}
			findings = append(findings, Finding{
				File: f.Rel, Line: line(f, sel.Pos()), Func: fnName,
				Rule:   "timer-rule",
				Detail: "time." + sel.Sel.Name + " is not on the §9.3a allowlist",
			})
		})
	}
	return findings
}

// StalePreconditionToken is the identifier a corroboration spawn must be
// guarded by. §4.2's T5 invariant: a rebuild triggered by a watch event
// refreshes corroboration only when the existing listing is older than
// staleAfterMs. Any other reachable path to a spawn is a poll in disguise.
const StalePreconditionToken = "staleafterms"

// AgentsSpawnGuard reports calls into the agents corroboration source that are
// not dominated by a comparison against staleAfterMs.
//
// What it checks, stated honestly because it is an approximation of dominance
// rather than dominance itself: the call must be lexically inside the BODY of
// an `if` whose condition mentions staleAfterMs. That is sound for the shape
// §4.2 specifies and it rejects the shape §4.2 forbids (an unguarded call on
// the Change branch). It would also reject a guard expressed as an early
// `return` — which is a real pattern, and the right response then is to add
// the dominance analysis, not to widen the rule, because an unguarded spawn is
// the failure this exists to catch.
//
// The agentscli adapter itself is exempt: it is the thing being called. So is
// every package in AgentsSpawnAllowlist, each with the citation that permits it.
func AgentsSpawnGuard(files []File) []Finding {
	var findings []Finding
	for _, f := range files {
		if f.IsTest || strings.Contains(f.Dir, "adapters/agentscli") || spawnAllowed(f.Dir) {
			continue
		}
		for _, decl := range f.AST.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			findings = append(findings, agentsSpawnsIn(f, fn.Name.Name, fn.Body, nil)...)
		}
	}
	return findings
}

// agentsSpawnsIn walks a statement tree carrying the stack of enclosing `if`
// conditions it is inside the body of.
func agentsSpawnsIn(f File, fnName string, n ast.Node, guards []string) []Finding {
	var findings []Finding
	switch node := n.(type) {
	case *ast.IfStmt:
		cond := exprText(f, node.Cond)
		findings = append(findings, agentsSpawnsIn(f, fnName, node.Body, append(guards, cond))...)
		if node.Else != nil {
			findings = append(findings, agentsSpawnsIn(f, fnName, node.Else, guards)...)
		}
		if node.Init != nil {
			findings = append(findings, agentsSpawnsIn(f, fnName, node.Init, guards)...)
		}
		return findings
	case *ast.BlockStmt:
		for _, stmt := range node.List {
			findings = append(findings, agentsSpawnsIn(f, fnName, stmt, guards)...)
		}
		return findings
	}

	guarded := false
	for _, g := range guards {
		if strings.Contains(strings.ToLower(g), StalePreconditionToken) {
			guarded = true
			break
		}
	}

	ast.Inspect(n, func(inner ast.Node) bool {
		switch node := inner.(type) {
		case *ast.IfStmt:
			findings = append(findings, agentsSpawnsIn(f, fnName, node, guards)...)
			return false
		case *ast.CallExpr:
			if !isAgentsListCall(node) {
				return true
			}
			if guarded {
				return true
			}
			findings = append(findings, Finding{
				File: f.Rel, Line: line(f, node.Pos()), Func: fnName,
				Rule: "t5-spawn-precondition",
				Detail: "a corroboration listing is taken without a " +
					"staleAfterMs comparison dominating it (§4.2 T5)",
			})
			return true
		}
		return true
	})
	return findings
}

// SpawnAllowlistEntry is one package permitted to take a corroboration listing
// outside T5's precondition, and the citation that permits it. The pair is the
// format §9.3(a) prescribes for every allowlist in this feature: a site without
// a citation is an exception nobody has had to justify.
type SpawnAllowlistEntry struct {
	Dir      string
	Citation string
}

// AgentsSpawnAllowlist is the deliberate exceptions to T5.
//
// T5 exists to stop a spawn happening on the WATCH PATH: a rebuild triggered by
// a filesystem event may refresh corroboration only when the listing it already
// has is older than staleAfterMs, because any other reachable path from an
// event to a spawn is a poll loop wearing a different hat.
//
// The doctor is not on that path and cannot get onto it. It is a one-shot,
// user-typed command; it holds no watch, subscribes to nothing, and takes
// exactly one listing per run (its own `agents` memo, asserted by a test in
// that package). An age gate there would be actively wrong: the doctor's job is
// to report whether the vendor CLI answers AND in what shape, and "the cached
// listing was fresh, so we did not look" is the one answer that check must
// never give.
var AgentsSpawnAllowlist = []SpawnAllowlistEntry{{
	Dir: "internal/observer/adapters/doctorfs",
	Citation: "§9.5 — the doctor takes exactly one listing per run for the " +
		"terminal-state gate, which §9.5 calls the check most likely to catch a " +
		"Claude behaviour change. It is user-initiated and off the watch path, so " +
		"T5's age gate does not apply and would suppress the observation.",
}}

// spawnAllowed reports whether a package directory is allowlisted.
func spawnAllowed(dir string) bool {
	for _, e := range AgentsSpawnAllowlist {
		if dir == e.Dir {
			return true
		}
	}
	return false
}

// isAgentsListCall recognises a call to the AgentsSource port's List method.
func isAgentsListCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "List" {
		return false
	}
	return strings.Contains(strings.ToLower(receiverText(sel.X)), "agents")
}

func receiverText(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return receiverText(v.X) + "." + v.Sel.Name
	case *ast.CallExpr:
		return receiverText(v.Fun)
	case *ast.IndexExpr:
		return receiverText(v.X)
	default:
		return ""
	}
}

// ---------------------------------------------------------------------------
// §5.1 — the omitempty and json-tag bans
// ---------------------------------------------------------------------------

// WirePackageDir is the one package allowed to carry json struct tags.
const WirePackageDir = "internal/observer/wire"

// OmitemptyAllow is one allowlisted omitempty, in the same (site, citation)
// format the timer allowlist uses.
type OmitemptyAllow struct {
	// JSONName is the field's json name.
	JSONName string
	Citation string
}

// OmitemptyAllowlist carries the single name §5.1 names, with its citation.
//
// Read carefully, because the architecture states the exception in terms of
// POINTERS and the ban in terms of TAGS, and they are not the same thing.
// §5.1: "A test walks wire's struct tags with go/ast and fails on any
// omitempty. The one deliberate non-pointer exception is waitReason (§3.3) …
// the test carries that single name as an allowlist entry with a citation,
// not as a silent exemption."
//
// The exception being described is that waitReason is a plain string where
// every other field with a meaningful absence is a pointer — NOT that
// waitReason may carry omitempty. It must not: `"waitReason": ""` is the JSON
// value for "not applicable", and omitempty would delete exactly that value.
//
// So the entry is here, named and cited as the rule requires, and the audit
// asserts the real tree needs it for nothing: zero omitempty tags exist, with
// or without the allowlist. TestWaitReasonIsTheOneAllowlistedException pins
// the property the exception actually refers to.
var OmitemptyAllowlist = []OmitemptyAllow{
	{
		JSONName: "waitReason",
		Citation: "§3.3 / §5.1: WaitReason is a non-pointer string. \"\" means " +
			"\"not applicable\" and \"unknown\" means \"waiting, reason " +
			"undetermined\", so a third null state would be meaningless — and " +
			"omitempty would erase the \"\" that carries the first meaning.",
	},
}

// Omitempty reports every struct tag carrying omitempty.
func Omitempty(files []File, allow []OmitemptyAllow) []Finding {
	allowed := make(map[string]bool, len(allow))
	for _, a := range allow {
		if strings.TrimSpace(a.Citation) != "" {
			allowed[a.JSONName] = true
		}
	}

	var findings []Finding
	for _, f := range files {
		eachStructTag(f, func(fnName, fieldName, tag string, pos token.Pos) {
			jsonTag, ok := reflect.StructTag(tag).Lookup("json")
			if !ok || !strings.Contains(jsonTag, "omitempty") {
				return
			}
			name := strings.Split(jsonTag, ",")[0]
			if name == "" {
				name = fieldName
			}
			if allowed[name] {
				return
			}
			findings = append(findings, Finding{
				File: f.Rel, Line: line(f, pos), Rule: "omitempty-ban",
				Detail: fmt.Sprintf("field %s carries omitempty (json %q): a null and a "+
					"zero are different answers in this domain", fieldName, jsonTag),
			})
		})
	}
	return findings
}

// JSONTags reports every json struct tag outside the wire package.
//
// The point is not tidiness. Four surfaces must serialise identical bytes from
// one snapshot; the conventional way to achieve that is for each adapter to
// build the same JSON, which drifts on the first field added. One set of tags
// in one package makes the agreement structural, so this audit is the thing
// that keeps AC11 true rather than merely currently-true.
func JSONTags(files []File, wireDirs ...string) []Finding {
	var findings []Finding
	for _, f := range files {
		if containsString(wireDirs, f.Dir) {
			continue
		}
		eachStructTag(f, func(fnName, fieldName, tag string, pos token.Pos) {
			if _, ok := reflect.StructTag(tag).Lookup("json"); !ok {
				return
			}
			findings = append(findings, Finding{
				File: f.Rel, Line: line(f, pos), Func: fnName, Rule: "json-tags-in-wire-only",
				Detail: fmt.Sprintf("field %s carries a json tag outside %s", fieldName, strings.Join(wireDirs, ", ")),
			})
		})
	}
	return findings
}

// CountJSONTags is how many json tags a file set carries, so a test can assert
// the walker is really seeing tags rather than silently visiting nothing.
func CountJSONTags(files []File) int {
	n := 0
	for _, f := range files {
		eachStructTag(f, func(_, _, tag string, _ token.Pos) {
			if _, ok := reflect.StructTag(tag).Lookup("json"); ok {
				n++
			}
		})
	}
	return n
}

// ---------------------------------------------------------------------------
// walking helpers
// ---------------------------------------------------------------------------

// walkWithFuncs visits every node, telling the callback which function it is
// inside. Package-level declarations report "", which matters: `var sleep =
// time.Sleep` is a reference at package scope and an audit that only walked
// function bodies would miss it.
func walkWithFuncs(f File, visit func(fnName string, n ast.Node)) {
	for _, decl := range f.AST.Decls {
		name := ""
		if fn, ok := decl.(*ast.FuncDecl); ok {
			name = fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) > 0 {
				name = receiverTypeName(fn.Recv.List[0].Type) + "." + name
			}
		}
		captured := name
		ast.Inspect(decl, func(n ast.Node) bool {
			if n != nil {
				visit(captured, n)
			}
			return true
		})
	}
}

func receiverTypeName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.StarExpr:
		return receiverTypeName(v.X)
	case *ast.Ident:
		return v.Name
	case *ast.IndexExpr:
		return receiverTypeName(v.X)
	default:
		return ""
	}
}

// eachStructTag visits every tagged struct field in a file, including structs
// declared inside function bodies and inside other structs.
func eachStructTag(f File, visit func(fnName, fieldName, tag string, pos token.Pos)) {
	walkWithFuncs(f, func(fnName string, n ast.Node) {
		st, ok := n.(*ast.StructType)
		if !ok || st.Fields == nil {
			return
		}
		for _, field := range st.Fields.List {
			if field.Tag == nil {
				continue
			}
			tag, err := strconv.Unquote(field.Tag.Value)
			if err != nil {
				continue
			}
			name := ""
			if len(field.Names) > 0 {
				name = field.Names[0].Name
			}
			visit(fnName, name, tag, field.Tag.Pos())
		}
	})
}

func line(f File, pos token.Pos) int { return f.Fset.Position(pos).Line }

func exprText(f File, e ast.Expr) string {
	start := f.Fset.Position(e.Pos())
	end := f.Fset.Position(e.End())
	if start.Filename != end.Filename || start.Offset >= end.Offset {
		return renderExpr(e)
	}
	// The source text is not available here (files are parsed from disk with a
	// nil src), so the expression is rendered from the AST instead. Rendering
	// is enough for a token search and avoids re-reading every file.
	return renderExpr(e)
}

// renderExpr produces a flat textual rendering of an expression, sufficient for
// searching it for an identifier.
func renderExpr(e ast.Expr) string {
	switch v := e.(type) {
	case nil:
		return ""
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return renderExpr(v.X) + "." + v.Sel.Name
	case *ast.BinaryExpr:
		return renderExpr(v.X) + " " + v.Op.String() + " " + renderExpr(v.Y)
	case *ast.UnaryExpr:
		return v.Op.String() + renderExpr(v.X)
	case *ast.ParenExpr:
		return "(" + renderExpr(v.X) + ")"
	case *ast.CallExpr:
		args := make([]string, 0, len(v.Args))
		for _, a := range v.Args {
			args = append(args, renderExpr(a))
		}
		return renderExpr(v.Fun) + "(" + strings.Join(args, ", ") + ")"
	case *ast.StarExpr:
		return "*" + renderExpr(v.X)
	case *ast.IndexExpr:
		return renderExpr(v.X) + "[" + renderExpr(v.Index) + "]"
	case *ast.BasicLit:
		return v.Value
	default:
		return ""
	}
}

func stringLiterals(args []ast.Expr) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		lit, ok := a.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			continue
		}
		if s, err := strconv.Unquote(lit.Value); err == nil {
			out = append(out, s)
		}
	}
	return out
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
