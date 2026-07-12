package channel

import (
	"fmt"
	"os"
)

// ResolveAttachmentData returns the raw bytes for an OutgoingAttachment,
// reading from disk when LocalPath is set. This avoids duplicating the
// resolution logic across every channel implementation.
//
// Returns an error if the attachment has neither Data nor LocalPath set.
func ResolveAttachmentData(att OutgoingAttachment) ([]byte, error) {
	if len(att.Data) > 0 {
		return att.Data, nil
	}
	if att.LocalPath != "" {
		data, err := os.ReadFile(att.LocalPath)
		if err != nil {
			return nil, fmt.Errorf("read %s from disk: %w", att.LocalPath, err)
		}
		return data, nil
	}
	return nil, fmt.Errorf("attachment %q has neither Data nor LocalPath", att.FileName)
}
