package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/krolaw/zipstream"
)

var (
	unpack = flag.Bool("unpack", false, "unpack downloaded file")
	source string
	target string
)

var (
	stdout      bool
	targetIsDir bool
	targetName  string
)

func usage() {
	fmt.Fprint(flag.CommandLine.Output(), "go-fetch [flags] <url> <target>\n")
	flag.PrintDefaults()
}

func main() {
	flag.Usage = usage
	flag.Parse()

	if len(flag.Args()) < 2 {
		usage()
		os.Exit(2)
	}

	source = flag.Arg(0)
	target = flag.Arg(1)
	stdout = target == "-"

	// is target a directory?
	if !stdout {
		fi, err := os.Stat(target)
		if err != nil && !os.IsNotExist(err) {
			panic(err) // can't access target
		}
		targetIsDir = fi != nil && fi.IsDir() || // target is a directory
			strings.HasSuffix(target, string(filepath.Separator)) // target should be a directory
	}

	// start download
	res, err := http.Get(source)
	if err != nil {
		panic(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		panic(res.Status)
	}

	// setup target file name
	if !stdout && targetIsDir {
		// try content disposition
		if disp := res.Header.Get("Content-Disposition"); disp != "" {
			if _, params, err := mime.ParseMediaType(disp); err != nil {
				targetName = params["filename"]
			}
		}

		// use the base name of the final URL, if it has an extension
		if targetName == "" {
			targetName = path.Base(res.Request.URL.Path)
		}

		// use base name from original use, since it's more predictable
		if len(path.Ext(targetName)) <= 1 {
			targetName = path.Base(source)
		}
	}

	if *unpack {
		unpackArchive(bufio.NewReader(res.Body))
	} else {
		writeAll(targetWriter(), res.Body)
	}
}

func targetWriter() io.WriteCloser {
	if stdout {
		return os.Stdout
	}
	if targetIsDir {
		target = filepath.Join(target, targetName)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		panic(err)
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		panic(err)
	}
	return f
}

func writeAll(w io.WriteCloser, r io.Reader) {
	_, err := io.Copy(w, r)
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		panic(err)
	}
}

func unpackArchive(r *bufio.Reader) {
	magic, _ := r.Peek(264)

	switch {
	case bytes.HasPrefix(magic, []byte("\x1f\x8b")):
		zr, err := gzip.NewReader(r)
		if err != nil {
			panic(err)
		}
		defer zr.Close()

		if zr.Name != "" {
			targetName = zr.Name
		} else {
			targetName = strings.TrimSuffix(targetName, ".gz")
		}

		unpackArchive(bufio.NewReader(zr))

	case bytes.HasPrefix(magic, []byte("BZh")):
		targetName = strings.TrimSuffix(targetName, ".bz2")
		br := bzip2.NewReader(r)
		unpackArchive(bufio.NewReader(br))

	case !stdout && bytes.HasPrefix(magic, []byte("PK")):
		unarchive(target, zipstream.NewReader(r))

	case !stdout && len(magic) > 257 && bytes.HasPrefix(magic[257:], []byte("ustar")):
		unarchive(target, tar.NewReader(r))

	default:
		writeAll(targetWriter(), r)
	}
}

func unarchive(dst string, a io.Reader) {
	for {
		name, fi, err := unarchiveNext(a)
		if err == io.EOF {
			return
		}
		if err != nil {
			panic(err)
		}

		target := filepath.Join(dst, name)

		switch {
		case fi.IsDir():
			if err := os.MkdirAll(target, fi.Mode()|0300); err != nil {
				panic(err)
			}

		case fi.Mode().IsRegular():
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fi.Mode())
			if err != nil {
				panic(err)
			}
			writeAll(f, a)

		case fi.Mode()&os.ModeSymlink != 0:
			old, err := ioutil.ReadAll(a)
			if err != nil {
				panic(err)
			}
			err = os.Symlink(string(old), target)
			if err != nil {
				panic(err)
			}
		}
	}
}

func unarchiveNext(a io.Reader) (string, os.FileInfo, error) {
	switch v := a.(type) {
	case *tar.Reader:
		h, err := v.Next()
		if err != nil {
			return "", nil, err
		}
		return h.Name, h.FileInfo(), nil

	case *zipstream.Reader:
		h, err := v.Next()
		if err != nil {
			return "", nil, err
		}
		return h.Name, h.FileInfo(), nil

	default:
		panic(fmt.Sprintf("unknown type %T", v))
	}
}
