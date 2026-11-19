package rootfs

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type entry struct {
	name     string
	typ      byte
	body     string
	mode     int64
	linkname string
}

func layer(t *testing.T, gz bool, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		mode := e.mode
		if mode == 0 {
			mode = 0o644
			if e.typ == tar.TypeDir {
				mode = 0o755
			}
		}
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: mode, Size: int64(len(e.body)), Linkname: e.linkname,
			ModTime: time.Unix(1700000000, 0), Uid: os.Getuid(), Gid: os.Getgid()}
		if e.typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if !gz {
		return buf.Bytes()
	}
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	zw.Write(buf.Bytes())
	zw.Close()
	return out.Bytes()
}

// The merged view of a chain is the directory that each layer is applied to in turn. On Linux
// the launcher applies to an overlay of the parents; here a plain directory has the same view.
func applyAll(t *testing.T, dir string, layers ...[]byte) {
	t.Helper()
	for _, l := range layers {
		if err := Apply(dir, bytes.NewReader(l)); err != nil {
			t.Fatal(err)
		}
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func TestChainID(t *testing.T) {
	a := "sha256:aa"
	b := "sha256:bb"
	if got := ChainIDs([]string{a})[0]; got != a {
		t.Fatalf("first chain is the diff ID, got %s", got)
	}
	sum := sha256.Sum256([]byte(a + " " + b))
	want := "sha256:" + hex.EncodeToString(sum[:])
	if got := ChainIDs([]string{a, b})[1]; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestHardLinkToParentFile(t *testing.T) {
	dir := t.TempDir()
	applyAll(t, dir,
		layer(t, true, entry{name: "bin/", typ: tar.TypeDir}, entry{name: "bin/tool", typ: tar.TypeReg, body: "tool", mode: 0o755}),
		layer(t, false, entry{name: "bin/alias", typ: tar.TypeLink, linkname: "bin/tool"}),
	)
	if read(t, filepath.Join(dir, "bin/alias")) != "tool" {
		t.Fatal("hard link does not reach the parent file")
	}
	a, _ := os.Stat(filepath.Join(dir, "bin/alias"))
	b, _ := os.Stat(filepath.Join(dir, "bin/tool"))
	if !os.SameFile(a, b) {
		t.Fatal("not a hard link")
	}
}

func TestDirectoryChangedInChildKeepsParentContent(t *testing.T) {
	dir := t.TempDir()
	applyAll(t, dir,
		layer(t, false, entry{name: "etc/", typ: tar.TypeDir, mode: 0o755}, entry{name: "etc/a", typ: tar.TypeReg, body: "a"}),
		layer(t, false, entry{name: "etc/", typ: tar.TypeDir, mode: 0o700}, entry{name: "etc/b", typ: tar.TypeReg, body: "b"}),
	)
	st, err := os.Stat(filepath.Join(dir, "etc"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o700 {
		t.Fatalf("mode %v, want 0700 from the child", st.Mode().Perm())
	}
	if !exists(filepath.Join(dir, "etc/a")) || !exists(filepath.Join(dir, "etc/b")) {
		t.Fatal("the child replaced the directory of the parent")
	}
}

func TestWhiteoutRemovesParentEntry(t *testing.T) {
	dir := t.TempDir()
	applyAll(t, dir,
		layer(t, false, entry{name: "x/", typ: tar.TypeDir}, entry{name: "x/gone", typ: tar.TypeReg, body: "1"}, entry{name: "x/kept", typ: tar.TypeReg, body: "2"}),
		layer(t, false, entry{name: "x/.wh.gone", typ: tar.TypeReg}),
	)
	if exists(filepath.Join(dir, "x/gone")) || exists(filepath.Join(dir, "x/.wh.gone")) {
		t.Fatal("whiteout not applied")
	}
	if !exists(filepath.Join(dir, "x/kept")) {
		t.Fatal("whiteout removed a sibling")
	}
}

func TestOpaqueDirectoryHidesParentButKeepsOwnEntries(t *testing.T) {
	dir := t.TempDir()
	applyAll(t, dir,
		layer(t, false, entry{name: "d/", typ: tar.TypeDir}, entry{name: "d/old", typ: tar.TypeReg, body: "old"}),
		// The child writes d/new before the opaque marker: the marker must not remove it.
		layer(t, false, entry{name: "d/", typ: tar.TypeDir}, entry{name: "d/new", typ: tar.TypeReg, body: "new"}, entry{name: "d/.wh..wh..opq", typ: tar.TypeReg}),
	)
	if exists(filepath.Join(dir, "d/old")) {
		t.Fatal("opaque directory kept a parent entry")
	}
	if !exists(filepath.Join(dir, "d/new")) {
		t.Fatal("opaque marker removed an entry of its own layer")
	}
	if exists(filepath.Join(dir, "d/.wh..wh..opq")) {
		t.Fatal("marker written as a file")
	}
}

func TestSymlinkAndReplaceFileByDirectory(t *testing.T) {
	dir := t.TempDir()
	applyAll(t, dir,
		layer(t, false, entry{name: "p", typ: tar.TypeReg, body: "file"}, entry{name: "l", typ: tar.TypeSymlink, linkname: "/p"}),
		layer(t, false, entry{name: "p/", typ: tar.TypeDir}),
	)
	if target, err := os.Readlink(filepath.Join(dir, "l")); err != nil || target != "/p" {
		t.Fatalf("symlink %q %v", target, err)
	}
	if st, err := os.Stat(filepath.Join(dir, "p")); err != nil || !st.IsDir() {
		t.Fatal("file not replaced by directory")
	}
}

func TestRefusesPathOutsideRoot(t *testing.T) {
	dir := t.TempDir()
	err := Apply(dir, bytes.NewReader(layer(t, false, entry{name: "../escape", typ: tar.TypeReg, body: "x"})))
	if err == nil {
		t.Fatal("accepted a path outside the root")
	}
	err = Apply(dir, bytes.NewReader(layer(t, false, entry{name: "a", typ: tar.TypeLink, linkname: "../../etc/passwd"})))
	if err == nil {
		t.Fatal("accepted a hard link outside the root")
	}
}

func TestRefusesZstd(t *testing.T) {
	err := Apply(t.TempDir(), bytes.NewReader([]byte{0x28, 0xb5, 0x2f, 0xfd, 0, 0, 0, 0}))
	if err == nil {
		t.Fatal("accepted a zstd layer")
	}
}
