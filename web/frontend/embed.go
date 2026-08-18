// Package frontend embeds the built web console (web/frontend/dist) into the
// Go binary so `tachi web` can serve it without a separate build step. The
// directory is otherwise a plain frontend source tree (vite/tsc ignore .go
// files); this file is what makes it a Go package.
//
// The embedded tree carries a "dist" prefix; web/server.go fs.Subs it before
// serving. An absent dist/ (no `npm run build` yet) is a compile-time error,
// same as the previous web/dist embedding.
package frontend

import "embed"

//go:embed dist
var DistFS embed.FS
