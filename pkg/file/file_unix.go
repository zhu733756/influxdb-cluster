//go:build !windows

package file

import (
	"errors"
	"os"
	"syscall"
)

func SyncDir(dirName string) error {
	// fsync the dir to flush the rename
	dir, err := os.OpenFile(dirName, os.O_RDONLY, os.ModeDir)
	if err != nil {
		return err
	}
	defer dir.Close()

	// While we're on unix, we may be running in a Docker container that is
	// pointed at a Windows volume over samba. That doesn't support fsyncs
	// on directories. This shows itself as an EINVAL, so we ignore that
	// error.
	err = dir.Sync()
	if pe, ok := err.(*os.PathError); ok && pe.Err == syscall.EINVAL {
		err = nil
	} else if err != nil {
		return err
	}

	return dir.Close()
}

// RenameFileWithReplacement will replace any existing file at newpath with the contents
// of oldpath. It works also if it the rename spans over several file systems.
//
// If no file already exists at newpath, newpath will be created using the contents
// of oldpath. If this function returns successfully, the contents of newpath will
// be identical to oldpath, and oldpath will be removed.
func RenameFileWithReplacement(oldpath, newpath string) error {
	if err := os.Rename(oldpath, newpath); !errors.Is(err, syscall.EXDEV) {
		// note: also includes err == nil
		return err
	}

	// move over filesystem boundaries, we have to copy.
	// (if there was another error, it will likely fail a second time)
	return MoveFileWithReplacement(oldpath, newpath)

}

// RenameFile renames oldpath to newpath, returning an error if newpath already
// exists. If this function returns successfully, the contents of newpath will
// be identical to oldpath, and oldpath will be removed.
func RenameFile(oldpath, newpath string) error {
	return RenameFileWithReplacement(oldpath, newpath)
}
