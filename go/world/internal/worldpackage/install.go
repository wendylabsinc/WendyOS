package worldpackage

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

var ErrInstallUncertain = errors.New("package may be installed but directory durability could not be confirmed")

// Install re-verifies all bundle bytes, then publishes a fully written private
// directory with an atomic no-replace rename. The parent must be trusted local
// storage. No script, model or actuator is executed. Inputs must not be mutated
// concurrently; Install's private verified copy is used for every write.
func Install(bundle Bundle, destination string) error {
	return install(bundle, destination, renameNoReplace)
}
func install(bundle Bundle, destination string, rename func(int, string, string) error) error {
	checked, e := Verify(bundle.Root, bundle.Files)
	if e != nil {
		return e
	}
	absolute, e := filepath.Abs(destination)
	if e != nil {
		return e
	}
	parentName, name := filepath.Dir(absolute), filepath.Base(absolute)
	if destination == "" || name == "." || name == ".." || absolute == parentName {
		return errors.New("invalid package destination")
	}
	parent, e := os.OpenRoot(parentName)
	if e != nil {
		return e
	}
	defer parent.Close()
	parentFD, e := parent.Open(".")
	if e != nil {
		return e
	}
	defer parentFD.Close()
	if _, e = parent.Lstat(name); e == nil {
		return os.ErrExist
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	entropy := make([]byte, 12)
	if _, e = rand.Read(entropy); e != nil {
		return e
	}
	staging := ".world-package-" + hex.EncodeToString(entropy)
	if e = parent.Mkdir(staging, 0700); e != nil {
		return e
	}
	defer parent.RemoveAll(staging)
	stage, e := parent.OpenRoot(staging)
	if e != nil {
		return e
	}
	defer stage.Close()
	directories := map[string]bool{".": true}
	write := func(name string, data []byte) error {
		dir := path.Dir(name)
		if err := stage.MkdirAll(dir, 0700); err != nil {
			return err
		}
		for part := dir; part != "."; part = path.Dir(part) {
			directories[part] = true
		}
		f, err := stage.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = f.Write(data)
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil {
			return err
		}
		return closeErr
	}
	if e = write("world.json", checked.Root); e != nil {
		return e
	}
	for _, file := range checked.Manifest.Files {
		if e = write(file.Path, checked.Files[file.Path]); e != nil {
			return e
		}
	}
	dirs := make([]string, 0, len(directories))
	for dir := range directories {
		dirs = append(dirs, dir)
	}
	sort.Slice(dirs, func(i, j int) bool {
		return strings.Count(dirs[i], "/") > strings.Count(dirs[j], "/") || strings.Count(dirs[i], "/") == strings.Count(dirs[j], "/") && dirs[i] > dirs[j]
	})
	for _, dir := range dirs {
		f, err := stage.Open(dir)
		if err != nil {
			return err
		}
		err = f.Sync()
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if e = rename(int(parentFD.Fd()), staging, name); e != nil {
		return e
	}
	if e = parentFD.Sync(); e != nil {
		return fmt.Errorf("%w: %v", ErrInstallUncertain, e)
	}
	return nil
}
