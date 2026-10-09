//go:build !darwin && !linux

package clipboard

import "context"

// Everywhere else the clipboard is out of reach: Windows' pasteboard needs an API this program does
// not link against, and a TUI there has the same text-only terminal channel (see the package doc).
// Said plainly instead of pretending the clipboard was empty.
func (r reader) image(context.Context) ([]byte, string, error) { return nil, "", ErrUnsupported }

func (r reader) text(context.Context) (string, error) { return "", ErrUnsupported }
