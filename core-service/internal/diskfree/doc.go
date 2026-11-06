// Package diskfree tells how much space is free on a disk (staging
// downloads, the archive's previews).
package diskfree

// Drive is a local drive: its root folder, and the space free to this user
// and in all.
type Drive struct {
	Root  string `json:"root"`
	Free  int64  `json:"free"`
	Total int64  `json:"total"`
}
