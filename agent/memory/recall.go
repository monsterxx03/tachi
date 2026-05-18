package memory

import (
	"fmt"
	"time"
)

// RelativeAge returns a human-readable age string like "(2 hours ago) ".
func RelativeAge(timestamp int64) string {
	t := time.Unix(timestamp, 0)
	d := time.Since(t)

	switch {
	case d < time.Minute:
		return "(just now) "
	case d < time.Hour:
		m := int(d.Minutes())
		return fmt.Sprintf("(%d minutes ago) ", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			return "(1 hour ago) "
		}
		return fmt.Sprintf("(%d hours ago) ", h)
	case d < 7*24*time.Hour:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "(1 day ago) "
		}
		return fmt.Sprintf("(%d days ago) ", days)
	case d < 30*24*time.Hour:
		weeks := int(d.Hours() / (24 * 7))
		if weeks == 1 {
			return "(1 week ago) "
		}
		return fmt.Sprintf("(%d weeks ago) ", weeks)
	default:
		months := int(d.Hours() / (24 * 30))
		if months == 1 {
			return "(1 month ago) "
		}
		return fmt.Sprintf("(%d months ago) ", months)
	}
}