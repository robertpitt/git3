package helper

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/robertpitt/git3/internal/engine"
	"github.com/robertpitt/git3/internal/store"
)

// Helper implements Git's remote-helper protocol for a Repository.
type Helper struct {
	Repo                 *engine.Repository
	In                   io.Reader
	Out, Err             io.Writer
	atomic, connectivity bool
	progress             bool
	verbosity            int
	dryRun               bool
	wantObjectFormat     bool
	requestedFormat      string
}

// Run serves remote-helper commands until standard input closes.
func (h *Helper) Run(ctx context.Context) error {
	sc := bufio.NewScanner(h.In)
	sc.Buffer(make([]byte, 64*1024), 128<<20)
	w := bufio.NewWriter(h.Out)
	defer w.Flush()
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "capabilities":
			fmt.Fprintln(w, "fetch")
			fmt.Fprintln(w, "push")
			fmt.Fprintln(w, "option")
			fmt.Fprintln(w, "check-connectivity")
			fmt.Fprintln(w, "object-format")
			fmt.Fprintln(w)
			w.Flush()
		case strings.HasPrefix(line, "option "):
			parts := strings.SplitN(line, " ", 3)
			if len(parts) < 3 {
				fmt.Fprintln(w, "error invalid option")
			} else {
				fmt.Fprintln(w, h.option(parts[1], parts[2]))
			}
			w.Flush()
		case line == "list" || line == "list for-push":
			s := h.Repo.Pinned
			var e error
			if s == nil {
				s, e = h.Repo.Read(ctx)
			}
			if e == store.ErrNotFound {
				fmt.Fprintln(w)
				w.Flush()
				continue
			}
			if e != nil {
				return e
			}
			if h.requestedFormat != "" && h.requestedFormat != s.Head.ObjectFormat {
				return fmt.Errorf("requested object format %s, remote uses %s", h.requestedFormat, s.Head.ObjectFormat)
			}
			if h.wantObjectFormat {
				fmt.Fprintf(w, ":object-format %s\n", s.Head.ObjectFormat)
			}
			for _, ref := range sorted(s.Refs) {
				fmt.Fprintf(w, "%s %s\n", s.Refs[ref], ref)
			}
			if s.Head.HeadSymref != nil {
				fmt.Fprintf(w, "@%s HEAD\n", *s.Head.HeadSymref)
			}
			fmt.Fprintln(w)
			w.Flush()
		case strings.HasPrefix(line, "fetch "):
			req := []string{}
			for {
				p := strings.Fields(line)
				if len(p) != 3 {
					return fmt.Errorf("invalid fetch command")
				}
				req = append(req, p[1])
				if !sc.Scan() {
					return io.ErrUnexpectedEOF
				}
				line = sc.Text()
				if line == "" {
					break
				}
				if !strings.HasPrefix(line, "fetch ") {
					return fmt.Errorf("expected fetch command")
				}
			}
			s := h.Repo.Pinned
			if s == nil {
				var e error
				s, e = h.Repo.Read(ctx)
				if e != nil {
					return e
				}
			}
			if e := h.Repo.Fetch(ctx, s, req, h.connectivity); e != nil {
				return e
			}
			for _, msg := range h.Repo.Warnings {
				fmt.Fprintln(h.Err, "warning:", msg)
			}
			h.Repo.Warnings = nil
			for _, p := range h.Repo.InstalledKeeps {
				fmt.Fprintf(w, "lock %s\n", p)
			}
			h.Repo.InstalledKeeps = nil
			if h.connectivity {
				fmt.Fprintln(w, "connectivity-ok")
			}
			fmt.Fprintln(w)
			w.Flush()
		case strings.HasPrefix(line, "push "):
			lines := []string{strings.TrimPrefix(line, "push ")}
			for {
				if !sc.Scan() {
					return io.ErrUnexpectedEOF
				}
				line = sc.Text()
				if line == "" {
					break
				}
				if !strings.HasPrefix(line, "push ") {
					return fmt.Errorf("expected push command")
				}
				lines = append(lines, strings.TrimPrefix(line, "push "))
			}
			cmds := make([]engine.PushCommand, 0, len(lines))
			for _, x := range lines {
				force := strings.HasPrefix(x, "+")
				x = strings.TrimPrefix(x, "+")
				p := strings.SplitN(x, ":", 2)
				if len(p) != 2 {
					return fmt.Errorf("invalid push refspec")
				}
				var oid *string
				if p[0] != "" {
					v, e := h.Repo.Git.Resolve(ctx, p[0])
					if e != nil {
						v = p[0]
					}
					oid = &v
				}
				cmds = append(cmds, engine.PushCommand{Source: p[0], Dst: p[1], NewOID: oid, Force: force})
			}
			res, e := h.Repo.Push(ctx, cmds, h.atomic)
			if e != nil {
				for _, c := range cmds {
					fmt.Fprintf(w, "error %s %s\n", c.Dst, sanitize(e.Error()))
				}
			} else {
				for _, x := range res {
					if x.OK {
						fmt.Fprintf(w, "ok %s\n", x.Dst)
					} else {
						fmt.Fprintf(w, "error %s %s\n", x.Dst, sanitize(x.Reason))
					}
				}
			}
			fmt.Fprintln(w)
			w.Flush()
		case line == "":
			continue
		default:
			return fmt.Errorf("unsupported remote-helper command %q", line)
		}
	}
	return sc.Err()
}
func (h *Helper) option(name, value string) string {
	parseBoolean := func() (bool, bool) {
		v, err := strconv.ParseBool(value)
		return v, err == nil
	}
	switch name {
	case "verbosity":
		n, e := strconv.Atoi(value)
		if e != nil || n < 0 {
			return "error verbosity must be non-negative"
		}
		h.verbosity = n
		return "ok"
	case "progress":
		v, ok := parseBoolean()
		if !ok {
			return "error invalid boolean"
		}
		h.progress = v
		return "ok"
	case "cloning", "force", "followtags":
		_, ok := parseBoolean()
		if !ok {
			return "error invalid boolean"
		}
		return "ok"
	case "check-connectivity":
		v, ok := parseBoolean()
		if !ok {
			return "error invalid boolean"
		}
		h.connectivity = v
		return "ok"
	case "atomic":
		v, ok := parseBoolean()
		if !ok {
			return "error invalid boolean"
		}
		h.atomic = v
		return "ok"
	case "dry-run":
		v, ok := parseBoolean()
		if !ok {
			return "error invalid boolean"
		}
		h.dryRun = v
		h.Repo.DryRun = v
		return "ok"
	case "object-format":
		if value == "true" {
			h.wantObjectFormat = true
			return "ok"
		}
		if value == "sha1" || value == "sha256" {
			h.wantObjectFormat = true
			h.requestedFormat = value
			return "ok"
		}
		return "error unsupported object format"
	case "depth", "deepen-since", "deepen-not", "deepen-relative", "update-shallow", "from-promisor", "no-dependents":
		return "error shallow and partial operation is unsupported"
	default:
		return "unsupported"
	}
}
func sorted(m map[string]string) []string {
	o := make([]string, 0, len(m))
	for k := range m {
		o = append(o, k)
	}
	for i := 1; i < len(o); i++ {
		for j := i; j > 0 && o[j] < o[j-1]; j-- {
			o[j], o[j-1] = o[j-1], o[j]
		}
	}
	return o
}
func sanitize(s string) string { return strings.NewReplacer("\n", " ", "\r", " ").Replace(s) }
