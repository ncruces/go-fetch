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
	"log"
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
	// parse command line args
	flag.Usage = usage
	flag.Parse()

	if len(flag.Args()) < 2 {
		usage()
		os.Exit(2)
	}

	source = flag.Arg(0)
	target = flag.Arg(1)
	stdout = target == "-"

	log.SetFlags(0)

	// is target a directory?
	if !stdout {
		if strings.HasSuffix(target, string(filepath.Separator)) {
			targetIsDir = true
		} else {
			fi, _ := os.Stat(target)
			targetIsDir = fi != nil && fi.IsDir()
		}
	}

	if abs, err := filepath.Abs(target); err != nil {
		log.Fatal(err)
	} else {
		target = abs
	}

	// start download
	res, err := http.Get(source)
	if err != nil {
		log.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		log.Fatal(res.Status)
	}

	// target file name
	if targetIsDir {
		// use content disposition
		if disp := res.Header.Get("Content-Disposition"); disp != "" {
			if _, params, err := mime.ParseMediaType(disp); err != nil {
				targetName = params["filename"]
			}
		}

		// use the base name of the final URL, if it has an extension
		if targetName == "" {
			targetName = path.Base(res.Request.URL.Path)
		}

		// use the base name of the source url, since it's more predictable
		if len(path.Ext(targetName)) <= 1 {
			targetName = path.Base(source)
		}
	}

	if *unpack {
		uncompress(bufio.NewReader(res.Body))
	} else {
		copy(targetFile(), res.Body)
	}
}

func targetFile() *os.File {
	if stdout {
		return os.Stdout
	}
	if targetIsDir {
		prefix := target + string(filepath.Separator)
		target = filepath.Join(target, targetName)
		if !strings.HasPrefix(target, prefix) {
			log.Fatalf("illegal file path: %s", targetName)
		}
	}
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		log.Fatal(err)
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		log.Fatal(err)
	}
	return f
}

func copy(w io.WriteCloser, r io.Reader) {
	_, err := io.Copy(w, r)
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		log.Fatal(err)
	}
}

func uncompress(r *bufio.Reader) {
	magic, _ := r.Peek(264)

	switch {
	case bytes.HasPrefix(magic, []byte("\x1f\x8b")):
		zr, err := gzip.NewReader(r)
		if err != nil {
			log.Fatal(err)
		}
		defer zr.Close()

		if zr.Name != "" {
			targetName = zr.Name
		} else {
			targetName = strings.TrimSuffix(targetName, ".gz")
		}

		uncompress(bufio.NewReader(zr))

	case bytes.HasPrefix(magic, []byte("BZh")):
		targetName = strings.TrimSuffix(targetName, ".bz2")
		br := bzip2.NewReader(r)
		uncompress(bufio.NewReader(br))

	case !stdout && bytes.HasPrefix(magic, []byte("PK")):
		unarchive(zipstream.NewReader(r))

	case !stdout && len(magic) > 257 && bytes.HasPrefix(magic[257:], []byte("ustar")):
		unarchive(tar.NewReader(r))

	default:
		copy(targetFile(), r)
	}
}

func unarchive(a io.Reader) {
	prefix := target + string(filepath.Separator)

	for {
		name, fi, err := unarchiveNext(a)
		if err == io.EOF {
			return
		}
		if err != nil {
			log.Fatal(err)
		}

		path := filepath.Join(target, name)
		if !strings.HasPrefix(path, prefix) {
			log.Fatalf("illegal file path: %s", name)
		}

		switch {
		case fi.IsDir():
			if err := os.MkdirAll(path, fi.Mode()|0300); err != nil {
				log.Fatal(err)
			}

		case fi.Mode().IsRegular():
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fi.Mode())
			if err != nil {
				log.Fatal(err)
			}
			copy(f, a)

		case fi.Mode()&os.ModeSymlink != 0:
			old, err := ioutil.ReadAll(a)
			if err != nil {
				log.Fatal(err)
			}
			err = os.Symlink(string(old), path)
			if err != nil {
				log.Fatal(err)
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
