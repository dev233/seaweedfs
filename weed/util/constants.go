package util

import (
	"fmt"
)

var (
	VERSION_NUMBER = fmt.Sprintf("%.02f", 3.52)
	VERSION        = sizeLimit + " " + VERSION_NUMBER
	COMMIT         = ""

	VERSIONMore = sizeLimit + " " + VERSION_NUMBER + " " + BuiltCommit + " remote, built: " + BuiltTime
	BuiltTime   string
	BuiltCommit string
)

func Version() string {
	return VERSIONMore + " " + COMMIT
}
