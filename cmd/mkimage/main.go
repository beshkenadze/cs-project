// mkimage writes the gzipped OCI layout archive of the launcher image. The image has one layer
// with the given files and fixed times, owners and order, so the same inputs give the same bytes.
//
//	mkimage -ref cs-project:0.1.0 -out image.tar.gz /cs-project=dist/cs-project /bin/runc=dist/runc ...
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

var epoch = time.Unix(0, 0).UTC()

type descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Platform    map[string]string `json:"platform,omitempty"`
}

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

func main() {
	ref := flag.String("ref", "", "image reference")
	out := flag.String("out", "", "output .tar.gz")
	flag.Parse()
	if *ref == "" || *out == "" || flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: mkimage -ref <ref> -out <file> /guest/path=host/path ...")
		os.Exit(125)
	}
	if err := run(*ref, *out, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "mkimage:", err)
		os.Exit(1)
	}
}

func run(ref, out string, specs []string) error {
	files := map[string]string{}
	for _, s := range specs {
		guest, host, ok := strings.Cut(s, "=")
		if !ok || !strings.HasPrefix(guest, "/") {
			return fmt.Errorf("bad file argument %q", s)
		}
		files[strings.TrimPrefix(guest, "/")] = host
	}
	layer, err := layerTar(files)
	if err != nil {
		return err
	}
	var gz bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&gz, gzip.BestCompression)
	zw.Write(layer)
	zw.Close()

	config, _ := json.Marshal(map[string]any{
		"architecture": "arm64", "os": "linux", "created": epoch.Format(time.RFC3339),
		"config": map[string]any{"Entrypoint": []string{"/cs-project", "init"}},
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{digest(layer)}},
	})
	manifest, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: digest(config), Size: int64(len(config))},
		"layers":        []descriptor{{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Digest: digest(gz.Bytes()), Size: int64(gz.Len())}},
	})
	index, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests": []descriptor{{
			MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: digest(manifest), Size: int64(len(manifest)),
			Platform:    map[string]string{"architecture": "arm64", "os": "linux"},
			Annotations: map[string]string{"org.opencontainers.image.ref.name": ref, "io.containerd.image.name": ref},
		}},
	})
	entries := []struct {
		name string
		data []byte
	}{
		{"oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`)},
		{"index.json", index},
		{"blobs/sha256/" + strings.TrimPrefix(digest(config), "sha256:"), config},
		{"blobs/sha256/" + strings.TrimPrefix(digest(manifest), "sha256:"), manifest},
		{"blobs/sha256/" + strings.TrimPrefix(digest(gz.Bytes()), "sha256:"), gz.Bytes()},
	}
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	for _, d := range []string{"blobs/", "blobs/sha256/"} {
		tw.WriteHeader(&tar.Header{Name: d, Typeflag: tar.TypeDir, Mode: 0o755, ModTime: epoch})
	}
	for _, e := range entries {
		tw.WriteHeader(&tar.Header{Name: e.name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(e.data)), ModTime: epoch})
		tw.Write(e.data)
	}
	tw.Close()
	var final bytes.Buffer
	fw, _ := gzip.NewWriterLevel(&final, gzip.BestCompression)
	fw.Write(archive.Bytes())
	fw.Close()
	if err := os.WriteFile(out, final.Bytes(), 0o644); err != nil {
		return err
	}
	fmt.Printf("%s %s layer %s\n", ref, digest(final.Bytes()), digest(layer))
	return nil
}

// layerTar makes the layer: each file with mode 0755 and its parent directories, plus the empty
// directories that the VM container mounts on.
func layerTar(files map[string]string) ([]byte, error) {
	dirs := map[string]bool{"data": true, "dev": true, "proc": true, "sys": true, "run": true, "tmp": true, "etc": true}
	for name := range files {
		for d := path.Dir(name); d != "."; d = path.Dir(d) {
			dirs[d] = true
		}
	}
	var names []string
	for d := range dirs {
		names = append(names, d+"/")
	}
	for f := range files {
		names = append(names, f)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, n := range names {
		if strings.HasSuffix(n, "/") {
			mode := int64(0o755)
			if n == "tmp/" {
				mode = 0o1777
			}
			if err := tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeDir, Mode: mode, ModTime: epoch}); err != nil {
				return nil, err
			}
			continue
		}
		data, err := os.ReadFile(files[n])
		if err != nil {
			return nil, err
		}
		if err := tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(data)), ModTime: epoch}); err != nil {
			return nil, err
		}
		tw.Write(data)
	}
	return buf.Bytes(), tw.Close()
}
