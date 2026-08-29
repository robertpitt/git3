package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Git runs native Git commands in Dir.
type Git struct{ Dir string }

func (g Git) cmd(ctx context.Context, args ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, "git", args...)
	environment := os.Environ()
	if g.Dir != "" {
		c.Dir = g.Dir
		environment = withoutRepositoryEnvironment(environment)
	}
	c.Env = append(environment, "GIT_OPTIONAL_LOCKS=0")
	return c
}

func withoutRepositoryEnvironment(environment []string) []string {
	// Git sets GIT_DIR for remote helpers. Those inherited paths are usually
	// relative to the caller's worktree and must not override an explicit Dir.
	repositoryVariables := map[string]bool{
		"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
		"GIT_COMMON_DIR":                   true,
		"GIT_DIR":                          true,
		"GIT_GRAFT_FILE":                   true,
		"GIT_INDEX_FILE":                   true,
		"GIT_OBJECT_DIRECTORY":             true,
		"GIT_PREFIX":                       true,
		"GIT_REPLACE_REF_BASE":             true,
		"GIT_SHALLOW_FILE":                 true,
		"GIT_WORK_TREE":                    true,
	}
	filtered := make([]string, 0, len(environment))
	for _, variable := range environment {
		name, _, _ := strings.Cut(variable, "=")
		if !repositoryVariables[name] {
			filtered = append(filtered, variable)
		}
	}
	return filtered
}

// Run executes Git with args and returns its standard output.
func (g Git) Run(ctx context.Context, args ...string) ([]byte, error) {
	c := g.cmd(ctx, args...)
	var errb bytes.Buffer
	c.Stderr = &errb
	out, e := c.Output()
	if e != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), e, strings.TrimSpace(errb.String()))
	}
	return out, nil
}

// Version returns the installed Git version string.
func (g Git) Version(ctx context.Context) (string, error) {
	b, e := g.Run(ctx, "--version")
	return strings.TrimSpace(string(b)), e
}

// RequireVersion reports whether Git meets the supplied minimum version.
func (g Git) RequireVersion(ctx context.Context, major, minor int) error {
	v, e := g.Version(ctx)
	if e != nil {
		return e
	}
	fields := strings.Fields(v)
	if len(fields) < 3 {
		return fmt.Errorf("unrecognized Git version %q", v)
	}
	parts := strings.Split(fields[2], ".")
	if len(parts) < 2 {
		return fmt.Errorf("unrecognized Git version %q", v)
	}
	ma, e1 := strconv.Atoi(parts[0])
	mi, e2 := strconv.Atoi(parts[1])
	if e1 != nil || e2 != nil || ma < major || ma == major && mi < minor {
		return fmt.Errorf("Git %d.%d or newer is required (found %s)", major, minor, v)
	}
	return nil
}

// ObjectFormat returns the repository's object hash format.
func (g Git) ObjectFormat(ctx context.Context) (string, error) {
	b, e := g.Run(ctx, "rev-parse", "--show-object-format")
	return strings.TrimSpace(string(b)), e
}

// GitPath resolves path within the repository's Git directory.
func (g Git) GitPath(ctx context.Context, path string) (string, error) {
	b, e := g.Run(ctx, "rev-parse", "--path-format=absolute", "--git-path", path)
	return strings.TrimSpace(string(b)), e
}

// Resolve returns the object ID named by rev.
func (g Git) Resolve(ctx context.Context, rev string) (string, error) {
	b, e := g.Run(ctx, "rev-parse", "--verify", rev+"^{object}")
	return strings.TrimSpace(string(b)), e
}

// HasObject reports whether oid exists locally.
func (g Git) HasObject(ctx context.Context, oid string) bool {
	return g.cmd(ctx, "cat-file", "-e", oid+"^{object}").Run() == nil
}

// IsAncestor reports whether old is an ancestor of new.
func (g Git) IsAncestor(ctx context.Context, old, new string) bool {
	return g.cmd(ctx, "merge-base", "--is-ancestor", old, new).Run() == nil
}

// CheckRef validates a ref name using Git's own rules.
func (g Git) CheckRef(ctx context.Context, ref string) error {
	return g.cmd(ctx, "check-ref-format", ref).Run()
}

// SymbolicHEAD returns the local symbolic HEAD target.
func (g Git) SymbolicHEAD(ctx context.Context) (string, error) {
	b, e := g.Run(ctx, "symbolic-ref", "-q", "HEAD")
	return strings.TrimSpace(string(b)), e
}

// VerifyConnectivity checks that all objects reachable from refs are present.
func (g Git) VerifyConnectivity(ctx context.Context, refs map[string]string) error {
	if len(refs) == 0 {
		return nil
	}
	c := g.cmd(ctx, "rev-list", "--objects", "--stdin", "--missing=print")
	in, e := c.StdinPipe()
	if e != nil {
		return e
	}
	var out, errb bytes.Buffer
	c.Stdout = &out
	c.Stderr = &errb
	if e = c.Start(); e != nil {
		return e
	}
	for _, oid := range refs {
		fmt.Fprintln(in, oid)
	}
	_ = in.Close()
	if e = c.Wait(); e != nil {
		return fmt.Errorf("connectivity: %w: %s", e, errb.String())
	}
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "?") {
			return fmt.Errorf("missing object %s", strings.TrimPrefix(sc.Text(), "?"))
		}
	}
	return sc.Err()
}

// ProcessReader streams command output and exposes its eventual exit status.
type ProcessReader struct {
	io.ReadCloser
	cmd    *exec.Cmd
	stderr *bytes.Buffer
}

// Wait closes the stream and waits for the command to exit.
func (p *ProcessReader) Wait() error {
	e := p.cmd.Wait()
	if e != nil {
		return fmt.Errorf("git pack-objects: %w: %s", e, p.stderr.String())
	}
	return nil
}

// PackObjects streams a pack for the positive revisions excluding negative revisions.
func (g Git) PackObjects(ctx context.Context, positive, negative []string, thin bool) (*ProcessReader, error) {
	args := []string{"pack-objects", "--revs", "--stdout"}
	if thin {
		args = append(args, "--thin")
	}
	c := g.cmd(ctx, args...)
	in, e := c.StdinPipe()
	if e != nil {
		return nil, e
	}
	out, e := c.StdoutPipe()
	if e != nil {
		return nil, e
	}
	errb := new(bytes.Buffer)
	c.Stderr = errb
	if e = c.Start(); e != nil {
		return nil, e
	}
	go func() {
		defer in.Close()
		for _, x := range positive {
			fmt.Fprintln(in, x)
		}
		for _, x := range negative {
			fmt.Fprintln(in, "^"+x)
		}
	}()
	return &ProcessReader{ReadCloser: out, cmd: c, stderr: errb}, nil
}

// IndexPack installs a streamed pack and returns its pack checksum.
func (g Git) IndexPack(ctx context.Context, r io.Reader, fixThin bool, keep string) (string, error) {
	args := []string{"index-pack", "--stdin", "--strict"}
	if fixThin {
		args = append(args, "--fix-thin")
	}
	if keep != "" {
		args = append(args, "--keep="+keep)
	}
	c := g.cmd(ctx, args...)
	c.Stdin = r
	var out, errb bytes.Buffer
	c.Stdout = &out
	c.Stderr = &errb
	if e := c.Run(); e != nil {
		return "", fmt.Errorf("index-pack: %w: %s", e, errb.String())
	}
	return strings.TrimSpace(out.String()), nil
}

// IndexPackFile builds an index for pack.
func (g Git) IndexPackFile(ctx context.Context, pack string) error {
	_, e := g.Run(ctx, "index-pack", "--strict", pack)
	return e
}

// VerifyPack checks the pack referenced by index.
func (g Git) VerifyPack(ctx context.Context, index string) error {
	_, e := g.Run(ctx, "verify-pack", "-v", index)
	return e
}

// CountPackObjects returns the object count recorded by index.
func (g Git) CountPackObjects(ctx context.Context, index string) (uint64, error) {
	f, e := os.Open(index)
	if e != nil {
		return 0, e
	}
	defer f.Close()
	c := g.cmd(ctx, "show-index")
	c.Stdin = f
	out, e := c.StdoutPipe()
	if e != nil {
		return 0, e
	}
	var errb bytes.Buffer
	c.Stderr = &errb
	if e = c.Start(); e != nil {
		return 0, e
	}
	var n uint64
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		n++
	}
	if e = sc.Err(); e != nil {
		return 0, e
	}
	if e = c.Wait(); e != nil {
		return 0, fmt.Errorf("show-index: %w: %s", e, errb.String())
	}
	return n, nil
}

// WriteMIDX refreshes the repository's multi-pack index.
func (g Git) WriteMIDX(ctx context.Context) error {
	return g.cmd(ctx, "multi-pack-index", "write").Run()
}

// IsComplete reports whether the repository is non-shallow and has no partial-clone filter.
func (g Git) IsComplete(ctx context.Context) error {
	for _, args := range [][]string{{"rev-parse", "--is-shallow-repository"}, {"config", "--get", "extensions.partialClone"}} {
		b, e := g.Run(ctx, args...)
		if e == nil && (strings.TrimSpace(string(b)) == "true" || len(bytes.TrimSpace(b)) > 0 && args[0] == "config") {
			return fmt.Errorf("shallow or partial repository")
		}
	}
	return g.cmd(ctx, "fsck", "--connectivity-only", "--no-dangling").Run()
}

// PackDir returns the repository's object pack directory.
func (g Git) PackDir(ctx context.Context) (string, error) {
	p, e := g.GitPath(ctx, "objects/pack")
	if e != nil {
		return "", e
	}
	return filepath.Clean(p), nil
}

// CreatePack writes a standalone pack and index into dir.
func (g Git) CreatePack(ctx context.Context, dir string, positive, negative []string) (string, string, error) {
	if e := os.MkdirAll(dir, 0700); e != nil {
		return "", "", e
	}
	base := filepath.Join(dir, "pack")
	c := g.cmd(ctx, "pack-objects", "--revs", base)
	in, e := c.StdinPipe()
	if e != nil {
		return "", "", e
	}
	var out, errb bytes.Buffer
	c.Stdout = &out
	c.Stderr = &errb
	if e = c.Start(); e != nil {
		return "", "", e
	}
	for _, x := range positive {
		fmt.Fprintln(in, x)
	}
	for _, x := range negative {
		fmt.Fprintln(in, "^"+x)
	}
	_ = in.Close()
	if e = c.Wait(); e != nil {
		return "", "", fmt.Errorf("pack-objects: %w: %s", e, errb.String())
	}
	sum := strings.TrimSpace(out.String())
	return base + "-" + sum + ".pack", base + "-" + sum + ".idx", nil
}

// MergePacks consolidates named packs into one pack and index in dir.
func (g Git) MergePacks(ctx context.Context, dir string, packNames []string) (string, string, error) {
	if e := os.MkdirAll(dir, 0700); e != nil {
		return "", "", e
	}
	base := filepath.Join(dir, "merge")
	c := g.cmd(ctx, "pack-objects", "--stdin-packs", base)
	in, e := c.StdinPipe()
	if e != nil {
		return "", "", e
	}
	var out, errb bytes.Buffer
	c.Stdout = &out
	c.Stderr = &errb
	if e = c.Start(); e != nil {
		return "", "", e
	}
	for _, n := range packNames {
		fmt.Fprintln(in, n)
	}
	_ = in.Close()
	if e = c.Wait(); e != nil {
		return "", "", fmt.Errorf("pack-objects --stdin-packs: %w: %s", e, errb.String())
	}
	sum := strings.TrimSpace(out.String())
	return base + "-" + sum + ".pack", base + "-" + sum + ".idx", nil
}
