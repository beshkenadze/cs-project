// Package rootfs unpacks image layers. Each layer is applied to the merged view of its parents
// (an overlay of the parent chain), so hard links to parent files and directory metadata from a
// parent are correct, and whiteouts become deletions in the merged view.
package rootfs

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	whiteoutPrefix = ".wh."
	opaqueMarker   = ".wh..wh..opq"
)

// ChainIDs gives the OCI chain ID of each prefix of the diff IDs.
func ChainIDs(diffIDs []string) []string {
	out := make([]string, len(diffIDs))
	for i, d := range diffIDs {
		if i == 0 {
			out[i] = d
			continue
		}
		sum := sha256.Sum256([]byte(out[i-1] + " " + d))
		out[i] = "sha256:" + hex.EncodeToString(sum[:])
	}
	return out
}

// Apply writes one layer (plain or gzip tar) to root, which is the merged view of its parents.
func Apply(root string, r io.Reader) error {
	br := bufio.NewReader(r)
	magic, _ := br.Peek(4)
	var src io.Reader = br
	switch {
	case bytes.HasPrefix(magic, []byte{0x1f, 0x8b}):
		zr, err := gzip.NewReader(br)
		if err != nil {
			return err
		}
		defer zr.Close()
		src = zr
	case bytes.Equal(magic, []byte{0x28, 0xb5, 0x2f, 0xfd}):
		return errors.New("zstd layers are not supported")
	}
	a := applier{root: root, written: map[string]bool{}}
	tr := tar.NewReader(src)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if err := a.entry(h, tr); err != nil {
			return fmt.Errorf("%s: %w", h.Name, err)
		}
	}
	// Directory times change while children are written, so they are set last, deepest first.
	// The name is resolved again: a later entry can have replaced a parent by a symbolic link.
	for i := len(a.dirs) - 1; i >= 0; i-- {
		d, b := path.Split(a.dirs[i].name)
		parent, err := a.resolveDir(d)
		if err != nil {
			continue
		}
		setTimes(filepath.Join(parent, b), a.dirs[i].header)
	}
	return nil
}

type dirTime struct {
	name   string
	header *tar.Header
}

type applier struct {
	root    string
	written map[string]bool
	dirs    []dirTime
}

func cleanName(name string) (string, error) {
	n := path.Clean("/" + name)
	if strings.Contains(name, "..") {
		for _, part := range strings.Split(name, "/") {
			if part == ".." {
				return "", errors.New("path leaves the root")
			}
		}
	}
	return strings.TrimPrefix(n, "/"), nil
}

func (a *applier) entry(h *tar.Header, r io.Reader) error {
	name, err := cleanName(h.Name)
	if err != nil {
		return err
	}
	if name == "" {
		return nil
	}
	dirName, base := path.Split(name)
	parent, err := a.resolveDir(dirName)
	if err != nil {
		return err
	}
	if base == opaqueMarker {
		return a.opaque(parent, strings.TrimSuffix(dirName, "/"))
	}
	if strings.HasPrefix(base, whiteoutPrefix) {
		hidden := strings.TrimPrefix(base, whiteoutPrefix)
		if hidden == "" || hidden == "." || hidden == ".." || strings.Contains(hidden, "/") {
			return errors.New("invalid whiteout")
		}
		return os.RemoveAll(filepath.Join(parent, hidden))
	}
	target := filepath.Join(parent, base)
	a.written[name] = true

	if h.Typeflag == tar.TypeDir {
		if st, err := os.Lstat(target); err == nil && !st.IsDir() {
			if err := os.RemoveAll(target); err != nil {
				return err
			}
		}
		if err := os.Mkdir(target, 0o700); err != nil && !os.IsExist(err) {
			return err
		}
	} else {
		if err := os.RemoveAll(target); err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeReg, tar.TypeRegA:
			f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, r)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.Symlink(h.Linkname, target); err != nil {
				return err
			}
		case tar.TypeLink:
			linkName, err := cleanName(h.Linkname)
			if err != nil {
				return err
			}
			ld, lb := path.Split(linkName)
			linkParent, err := a.resolveDir(ld)
			if err != nil {
				return err
			}
			return os.Link(filepath.Join(linkParent, lb), target)
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			mode := uint32(h.Mode & 0o7777)
			switch h.Typeflag {
			case tar.TypeChar:
				mode |= unix.S_IFCHR
			case tar.TypeBlock:
				mode |= unix.S_IFBLK
			default:
				mode |= unix.S_IFIFO
			}
			if err := unix.Mknod(target, mode, int(unix.Mkdev(uint32(h.Devmajor), uint32(h.Devminor)))); err != nil {
				return err
			}
		default:
			return nil
		}
	}

	if err := os.Lchown(target, h.Uid, h.Gid); err != nil && os.Geteuid() == 0 {
		return err
	}
	for key, value := range h.PAXRecords {
		if attr, ok := strings.CutPrefix(key, "SCHILY.xattr."); ok {
			if err := unix.Lsetxattr(target, attr, []byte(value), 0); err != nil && os.Geteuid() == 0 {
				return err
			}
		}
	}
	if h.Typeflag != tar.TypeSymlink {
		// After chown: chown clears the set-user-ID and set-group-ID bits.
		if err := os.Chmod(target, os.FileMode(h.Mode&0o777)|specialBits(h.Mode)); err != nil {
			return err
		}
	}
	if h.Typeflag == tar.TypeDir {
		a.dirs = append(a.dirs, dirTime{name, h})
	} else {
		setTimes(target, h)
	}
	return nil
}

// opaque removes the entries of dir that come from the parents. Entries that this layer wrote
// before the marker stay.
func (a *applier) opaque(dir, name string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		rel := e.Name()
		if name != "" {
			rel = name + "/" + e.Name()
		}
		if !a.written[rel] {
			if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// resolveDir gives the host path of a directory of the layer. A symbolic link in it is followed
// inside the root only, as the image sees it.
func (a *applier) resolveDir(dir string) (string, error) {
	current := ""
	parts := strings.Split(strings.Trim(dir, "/"), "/")
	for hops := 0; len(parts) > 0; {
		part := parts[0]
		parts = parts[1:]
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			current = path.Dir(current)
			if current == "." {
				current = ""
			}
			continue
		}
		next := path.Join(current, part)
		st, err := os.Lstat(filepath.Join(a.root, next))
		if err != nil {
			if os.IsNotExist(err) {
				// A parent that the tar did not list: Docker creates it.
				if err := os.Mkdir(filepath.Join(a.root, next), 0o755); err != nil {
					return "", err
				}
				current = next
				continue
			}
			return "", err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			hops++
			if hops > 40 {
				return "", errors.New("too many symbolic links")
			}
			link, err := os.Readlink(filepath.Join(a.root, next))
			if err != nil {
				return "", err
			}
			if strings.HasPrefix(link, "/") {
				current = ""
			}
			parts = append(strings.Split(link, "/"), parts...)
			continue
		}
		if !st.IsDir() {
			return "", fmt.Errorf("%s is not a directory", next)
		}
		current = next
	}
	return filepath.Join(a.root, current), nil
}

func specialBits(mode int64) os.FileMode {
	var m os.FileMode
	if mode&0o4000 != 0 {
		m |= os.ModeSetuid
	}
	if mode&0o2000 != 0 {
		m |= os.ModeSetgid
	}
	if mode&0o1000 != 0 {
		m |= os.ModeSticky
	}
	return m
}

func setTimes(target string, h *tar.Header) {
	at := h.AccessTime
	if at.IsZero() {
		at = h.ModTime
	}
	ts := []unix.Timespec{unix.NsecToTimespec(at.UnixNano()), unix.NsecToTimespec(h.ModTime.UnixNano())}
	if h.ModTime.IsZero() {
		now := unix.NsecToTimespec(time.Now().UnixNano())
		ts = []unix.Timespec{now, now}
	}
	_ = unix.UtimesNanoAt(unix.AT_FDCWD, target, ts, unix.AT_SYMLINK_NOFOLLOW)
}
