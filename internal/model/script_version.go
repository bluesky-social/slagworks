package model

import "time"

// ScriptVersion is one immutable version of a storage or network script.
// Every upload mints version 1; every update mints head+1 and
// never touches an earlier version. The history endpoints/CLI use this to
// make the versioning observable: Head marks which version is currently
// served.
type ScriptVersion struct {
	Version   int       `json:"version"`
	Priority  int       `json:"priority"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
	Head      bool      `json:"head"`
}
