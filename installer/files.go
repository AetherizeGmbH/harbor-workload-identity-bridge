// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// maxHostFileSize bounds every read of a host file. The largest file the
// installer handles is the plugin binary (about 10 MiB).
const maxHostFileSize = 64 << 20

// Every host file is opened relative to its directory (os.Root), and its
// last path component must be a regular file, never a symlink. The sync
// container, and any pod with a hostPath volume on the same directory,
// can write next to the files this privileged installer reads and
// replaces. Before this check a planted symlink such as
// `harbor-bridge-ca.crt.bak -> ../../cron.d/x` turned the next install
// into a root write (or, for the read side, a copy of any host file into
// a world-readable backup) anywhere on the node. plugin.hostConfigDir
// itself is opened component by component (openNodeDir): a release's
// plugin.hostConfigDir can sit inside another release's, whose sync
// container could otherwise replace it with a symlink.

// readHostFile returns the content of the regular file at path. A missing
// file returns an error that matches fs.ErrNotExist.
func readHostFile(path string) ([]byte, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return readFileIn(root, filepath.Base(path))
}

// readFileIn returns the content of the regular file name in root. A
// missing file returns an error that matches fs.ErrNotExist.
func readFileIn(root *os.Root, name string) ([]byte, error) {
	f, err := openRegular(root, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return readAllCapped(f)
}

// openRegular opens name in root for reading and returns an error unless
// it is a regular file within the size cap. os.Root blocks symlinks that
// leave the directory but follows ones that stay inside it, so the last
// component is checked with Lstat first, and the opened file must be the
// same file (no swap between the check and the open). O_NONBLOCK keeps a
// FIFO swapped in at that moment from blocking the open.
func openRegular(root *os.Root, name string) (*os.File, error) {
	return openRegularMax(root, name, maxHostFileSize)
}

// openRegularMax is openRegular with the size cap maxSize.
func openRegularMax(root *os.Root, name string, maxSize int64) (*os.File, error) {
	lfi, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if lfi.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("refusing to follow symlink %s in %s", name, root.Name())
	}
	if !lfi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s in %s is not a regular file (%s)", name, root.Name(), lfi.Mode().Type())
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !os.SameFile(lfi, fi) {
		_ = f.Close()
		return nil, fmt.Errorf("%s in %s changed while it was opened", name, root.Name())
	}
	if fi.Size() > maxSize {
		_ = f.Close()
		return nil, fmt.Errorf("%s in %s is larger than %d bytes", name, root.Name(), maxSize)
	}
	return f, nil
}

// hashHostFile returns the hex SHA-256 of the regular file at path, opened
// under the rules of readHostFile and read in a stream: the kubelet binary
// (kubeletIdentity) is larger than maxHostFileSize. It refuses a file
// larger than maxSize.
func hashHostFile(path string, maxSize int64) (string, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	f, err := openRegularMax(root, filepath.Base(path), maxSize)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, maxSize+1))
	if err != nil {
		return "", err
	}
	if n > maxSize {
		return "", fmt.Errorf("%s is larger than %d bytes", path, maxSize)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func readAllCapped(f *os.File) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(f, maxHostFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxHostFileSize {
		return nil, fmt.Errorf("%s is larger than %d bytes", f.Name(), maxHostFileSize)
	}
	return data, nil
}

// isExecutableHostFile reports whether path is a regular file with an
// execute bit, without following a symlink in its last component.
func isExecutableHostFile(path string) bool {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return false
	}
	defer func() { _ = root.Close() }()
	fi, err := root.Lstat(filepath.Base(path))
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}

// isRegularHostFile reports whether path is a regular file, without
// following a symlink in its last component.
func isRegularHostFile(path string) bool {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return false
	}
	defer func() { _ = root.Close() }()
	fi, err := root.Lstat(filepath.Base(path))
	return err == nil && fi.Mode().IsRegular()
}

// mkdirNodeDir creates the node directory dir (a host path) and its
// parents when missing, with the node's usual mode (/etc/kubernetes is
// 0755). It follows symlinks: the directories it is used for must be
// outside what a pod on the node can write. plugin.hostConfigDir, which
// every release's sync container writes, and whose parent can be another
// release's plugin.hostConfigDir, goes through openNodeDir instead.
func mkdirNodeDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	return nil
}

// openNodeDir opens the node directory nodeDir (an absolute, clean node
// path) under hostRoot, the container path of the node's "/", and creates
// it and its missing parents with the node's usual mode (0755). Every
// component of nodeDir must be a directory, never a symlink: os.MkdirAll
// and os.OpenRoot follow symlinks in a directory path, and os.Root follows
// one that stays inside the directory it was opened on. A pod that can
// write a parent of nodeDir (for plugin.hostConfigDir, the sync container
// of a release whose plugin.hostConfigDir contains it) could otherwise
// replace nodeDir with a symlink and send this installer's root writes into
// a directory of its choice. Each component is opened relative to the one
// before it and must be the directory Lstat saw (os.SameFile), so a swap
// between the check and the open fails too. hostRoot itself is trusted.
func openNodeDir(hostRoot, nodeDir string) (*os.Root, error) {
	root, err := os.OpenRoot(hostRoot)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", hostRoot, err)
	}
	for _, name := range strings.Split(strings.TrimPrefix(nodeDir, "/"), "/") {
		if name == "" {
			continue
		}
		next, err := openSubdir(root, name)
		_ = root.Close()
		if err != nil {
			return nil, fmt.Errorf("open node directory %s: %w", nodeDir, err)
		}
		root = next
	}
	return root, nil
}

// openSubdir opens the directory name in parent, creating it when missing,
// and refuses a symlink or anything else that is not a directory
// (openNodeDir).
func openSubdir(parent *os.Root, name string) (*os.Root, error) {
	lfi, err := parent.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		if err := parent.Mkdir(name, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		lfi, err = parent.Lstat(name)
	}
	if err != nil {
		return nil, err
	}
	if lfi.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("refusing to follow symlink %s in %s", name, parent.Name())
	}
	if !lfi.IsDir() {
		return nil, fmt.Errorf("%s in %s is not a directory (%s)", name, parent.Name(), lfi.Mode().Type())
	}
	dir, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	fi, err := dir.Stat(".")
	if err != nil {
		_ = dir.Close()
		return nil, err
	}
	if !os.SameFile(lfi, fi) {
		_ = dir.Close()
		return nil, fmt.Errorf("%s in %s changed while it was opened", name, parent.Name())
	}
	return dir, nil
}

// writeFileAtomic writes data to path via a same-directory temp file +
// rename so a reader (kubelet, containerd) never sees a torn write.
// When replacing different content it first preserves the old bytes at
// path+".bak". Returns whether the on-disk content changed. All
// operations are relative to the directory and never follow a symlink in
// the last component (see readHostFile); rename replaces a symlink
// instead of writing through it. For the installer's own files: they get
// mode perm, and the installer's owner (writeForeignFile for other files).
func writeFileAtomic(path string, data []byte, perm os.FileMode) (changed bool, err error) {
	root, err := openParent(path)
	if err != nil {
		return false, err
	}
	defer func() { _ = root.Close() }()
	return writeFileIn(root, filepath.Base(path), data, perm)
}

// writeForeignFile is writeFileAtomic for a node file the installer edits
// but does not own (writeForeignIn).
func writeForeignFile(path string, data []byte) (changed bool, err error) {
	root, err := openParent(path)
	if err != nil {
		return false, err
	}
	defer func() { _ = root.Close() }()
	return writeForeignIn(root, filepath.Base(path), data)
}

// openParent opens the directory of the host path path, creating it when
// missing (mkdirNodeDir).
func openParent(path string) (*os.Root, error) {
	dir := filepath.Dir(path)
	if err := mkdirNodeDir(dir); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dir, err)
	}
	return root, nil
}

// fileMeta is the permission bits and owner a written file gets.
type fileMeta struct {
	perm os.FileMode
	// owner is nil for the installer's own (root on the node).
	owner *fileOwner
}

type fileOwner struct{ uid, gid int }

// ownerOf returns the owner of the file fi describes.
func ownerOf(fi os.FileInfo) *fileOwner {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return &fileOwner{uid: int(st.Uid), gid: int(st.Gid)}
}

// writeFileIn is writeFileAtomic for the file name in the directory root.
// The file gets mode perm, also when its content is current (a prior
// version may have written it with other permissions).
func writeFileIn(root *os.Root, name string, data []byte, perm os.FileMode) (changed bool, err error) {
	return writeIn(root, name, data, func(os.FileInfo) fileMeta { return fileMeta{perm: perm} })
}

// foreignFileMode is the mode of a node file the installer edits but does
// not own when it creates it, and the most it leaves on one that exists
// (writeForeignIn).
const foreignFileMode os.FileMode = 0o644

// writeForeignIn is writeFileIn for a node file that the installer edits
// but does not own: kubelet's credential-provider config in merge mode
// (usually the cloud's) and the kubelet environment file in patch mode.
// An existing file keeps its owner and group and its permission bits,
// capped at foreignFileMode, and so does its .bak: the installer never
// makes it readable by more users than it was, nor group- or
// world-writable. A file that did not exist gets foreignFileMode.
func writeForeignIn(root *os.Root, name string, data []byte) (changed bool, err error) {
	return writeIn(root, name, data, func(existing os.FileInfo) fileMeta {
		if existing == nil {
			return fileMeta{perm: foreignFileMode}
		}
		return fileMeta{perm: existing.Mode().Perm() & foreignFileMode, owner: ownerOf(existing)}
	})
}

// writeIn writes data as the file name in root (writeFileAtomic), with the
// metadata meta returns for the file as it is (nil when there is none).
// That metadata also applies to the .bak and, when the content is current,
// to the file itself.
func writeIn(root *os.Root, name string, data []byte, meta func(existing os.FileInfo) fileMeta) (changed bool, err error) {
	path := filepath.Join(root.Name(), name)
	f, err := openRegular(root, name)
	var m fileMeta
	switch {
	case err == nil:
		fi, serr := f.Stat()
		if serr != nil {
			_ = f.Close()
			return false, fmt.Errorf("stat %s: %w", path, serr)
		}
		m = meta(fi)
		old, rerr := readAllCapped(f)
		if rerr != nil {
			_ = f.Close()
			return false, fmt.Errorf("read %s: %w", path, rerr)
		}
		if bytes.Equal(old, data) {
			// Content is current; still enforce the mode. fchmod on the
			// open file, never chmod by name.
			var cerr error
			if fi.Mode()&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != m.perm {
				cerr = f.Chmod(m.perm)
			}
			_ = f.Close()
			if cerr != nil {
				return false, fmt.Errorf("chmod %s: %w", path, cerr)
			}
			return false, nil
		}
		_ = f.Close()
		if err := replaceFile(root, name+".bak", old, m); err != nil {
			return false, fmt.Errorf("write backup %s.bak: %w", path, err)
		}
	case errors.Is(err, fs.ErrNotExist):
		// First write — no backup.
		m = meta(nil)
	default:
		return false, fmt.Errorf("read %s: %w", path, err)
	}

	if err := replaceFile(root, name, data, m); err != nil {
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	return true, nil
}

// replaceFile atomically replaces name in root with data: a fresh
// O_EXCL temp file, fchown (when m names an owner), fchmod, fsync,
// rename, then fsync of the directory. The syncs matter because these
// files decide whether kubelet starts: without them a power loss right
// after the install can leave an empty or truncated config that kubelet
// refuses at boot.
func replaceFile(root *os.Root, name string, data []byte, m fileMeta) error {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return fmt.Errorf("random temp suffix: %w", err)
	}
	tmpName := name + ".tmp-" + hex.EncodeToString(suffix[:])
	tmp, err := root.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer func() { _ = root.Remove(tmpName) }() // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if m.owner != nil {
		// Before the chmod: a chown can clear mode bits.
		if err := chownIfOther(tmp, *m.owner); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("chown %s: %w", tmpName, err)
		}
	}
	if err := tmp.Chmod(m.perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := root.Rename(tmpName, name); err != nil {
		return fmt.Errorf("rename %s → %s: %w", tmpName, name, err)
	}
	return syncDir(root)
}

// chownIfOther gives f the owner o unless it has it already.
func chownIfOther(f *os.File, o fileOwner) error {
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if cur := ownerOf(fi); cur != nil && *cur == o {
		return nil
	}
	return f.Chown(o.uid, o.gid)
}

// priorContent is a node file as a pass found it before its own write,
// which a rollback restores (ADR-0033).
type priorContent struct {
	existed bool
	data    []byte
	meta    fileMeta // its permission bits and owner
}

// readPrior reads the file name in root as priorContent. A missing file did
// not exist; anything but a regular file is an error (openRegular).
func readPrior(root *os.Root, name string) (priorContent, error) {
	f, err := openRegular(root, name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return priorContent{}, nil
	case err != nil:
		return priorContent{}, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return priorContent{}, err
	}
	data, err := readAllCapped(f)
	if err != nil {
		return priorContent{}, err
	}
	return priorContent{existed: true, data: data, meta: fileMeta{perm: fi.Mode().Perm(), owner: ownerOf(fi)}}, nil
}

// readPriorHostFile is readPrior for the host path path; a missing
// directory is a missing file.
func readPriorHostFile(path string) (priorContent, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return priorContent{}, nil
	case err != nil:
		return priorContent{}, err
	}
	defer func() { _ = root.Close() }()
	return readPrior(root, filepath.Base(path))
}

// restoreIn puts the file name in root back as p describes it: the same
// content, permission bits and owner, written like any other (writeIn, so
// its .bak then holds the content being replaced), or no file when it did
// not exist.
func restoreIn(root *os.Root, name string, p priorContent) error {
	if p.existed {
		_, err := writeIn(root, name, p.data, func(os.FileInfo) fileMeta { return p.meta })
		return err
	}
	return removeRegularIn(root, name)
}

// restoreHostFile is restoreIn for the host path path.
func restoreHostFile(path string, p priorContent) error {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open %s: %w", filepath.Dir(path), err)
	}
	defer func() { _ = root.Close() }()
	return restoreIn(root, filepath.Base(path), p)
}

// removeRegularIn removes the file name in root, when there is one, and
// syncs the directory: kubelet must not find the file again after a power
// loss. Anything but a regular file is left alone and is an error. Remove
// unlinks the name itself, so a symlink swapped in after the check is
// removed, never followed.
func removeRegularIn(root *os.Root, name string) error {
	path := filepath.Join(root.Name(), name)
	lfi, err := root.Lstat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case !lfi.Mode().IsRegular():
		return fmt.Errorf("refusing to remove %s: not a regular file (%s)", path, lfi.Mode().Type())
	}
	if err := root.Remove(name); err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return syncDir(root)
}

// syncDir fsyncs the directory root, making renames and removals in it
// durable.
func syncDir(root *os.Root) error {
	d, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("open %s for sync: %w", root.Name(), err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", root.Name(), err)
	}
	return nil
}

// ensureWritableDir creates dir if needed and proves a file can be
// created in it.
func ensureWritableDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	f, err := os.CreateTemp(dir, ".write-probe-*")
	if err != nil {
		return fmt.Errorf("not writable: %w", err)
	}
	name := f.Name()
	_ = f.Close()
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("remove write probe: %w", err)
	}
	return nil
}

// copyFile copies src to dst atomically, returning whether dst changed.
func copyFile(src, dst string, perm os.FileMode) (bool, error) {
	data, err := os.ReadFile(src)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", src, err)
	}
	return writeFileAtomic(dst, data, perm)
}

// copyFileIn copies src to the file name in the directory root atomically,
// returning whether that file changed.
func copyFileIn(src string, root *os.Root, name string, perm os.FileMode) (bool, error) {
	data, err := os.ReadFile(src)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", src, err)
	}
	return writeFileIn(root, name, data, perm)
}
