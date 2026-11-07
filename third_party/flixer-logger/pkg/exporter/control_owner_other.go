//go:build !unix

package exporter

import "os"

func socketOwnedByDirectory(_, _ os.FileInfo) bool {
	return false
}
