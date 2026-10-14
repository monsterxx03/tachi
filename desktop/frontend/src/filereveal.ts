// The one thing the CONVERSATION can ask the FILE TREE to do: show me where this file lives.
//
// A reply's file link, an attachment card and the tree itself are three different modules, and the
// gesture is a single sentence — "expand to this path and select its row". It is passed through a
// module-level slot rather than a React context or a prop chain because the app has exactly ONE tree:
// a provider per render, plumbed through react-markdown's component map, would be a lot of machinery
// for a single subscriber. It is its own module rather than a pair of exports from filetree.tsx so
// that the subscribers (markdown.tsx, filepreview.tsx) do not import the whole tree panel — which
// imports the preview module back.
//
// No tree open means the request is dropped, on purpose: opening a column of its own accord because
// the reader clicked a link is exactly the kind of helpfulness this app avoids.

let revealHandler: ((path: string) => void) | null = null

// onRevealFile registers the tree's handler for as long as the tree is mounted; the returned
// function unregisters it.
export function onRevealFile(fn: (path: string) => void): () => void {
	revealHandler = fn
	return () => {
		if (revealHandler === fn) revealHandler = null
	}
}

// revealFileInTree asks the open tree to expand to a path and select it. It never opens anything: the
// reader clicked a link and already got the file in an overlay, and the tree's answer is where it is.
export function revealFileInTree(path: string): void {
	if (path) revealHandler?.(path)
}
