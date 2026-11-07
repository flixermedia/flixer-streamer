//go:build unix

package exporter

import (
	"os"
	"syscall"
)

func socketOwnedByDirectory(dir, socket os.FileInfo) bool {
	parentOwner, parentOK := dir.Sys().(*syscall.Stat_t)
	socketOwner, socketOK := socket.Sys().(*syscall.Stat_t)
	return parentOK && socketOK && parentOwner.Uid == socketOwner.Uid
}
