package tools

import (
	"context"
	"path/filepath"

	"github.com/monsterxx03/tachi/agent/wdctx"
)

// ResolvePath turns a path argument into the absolute-ish path the tool acts on:
// an absolute path is used as it is, a relative one is joined to the run's working
// directory (wdctx). It is the ONE implementation of that rule.
//
// Why it is one function: this join is the seam any change to "what a relative
// path means" has to pass through. Today the working directory is the only root, so
// the rule is short — but it was previously open-coded in every path-taking tool
// (read / write / edit / rg / sendfile / lsp), which meant a rule change was a
// hunt for copies. Callers keep their own follow-up steps (rg wants an absolute
// path, the LSP tools clean before building a file:// URI).
//
// What is NOT this function's business: the working directory used as a ROOT rather
// than as a base for a user-supplied path — bash's cwd, the LSP workspace root, the
// plans directory, the ACP terminal cwd. Those stay primary-only by design; they
// read wdctx.Dir directly.
func ResolvePath(ctx context.Context, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(wdctx.Dir(ctx), p)
}
