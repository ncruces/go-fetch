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
	"net/url"
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

	// start download
	res, err := http.Get(source)
	if err != nil {
		log.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		log.Fatal("http error: ", res.Status)
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
			u, _ := url.Parse(source)
			targetName = path.Base(u.Path)
		}
	}

	if *unpack {
		err = uncompress(bufio.NewReader(res.Body))
	} else {
		err = write(res.Body, targetFile())
	}
	if err != nil {
		log.Fatal(err)
	}
}

func targetFile() *os.File {
	if stdout {
		return os.Stdout
	}

	path := target
	if targetIsDir {
		name := filepath.FromSlash(targetName)
		if strings.ContainsRune(name, filepath.Separator) {
			log.Fatalf("illegal file path: %q", targetName)
		}
		path = filepath.Join(path, name)
	}

	path, err := filepath.Abs(path)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0777); err != nil {
		log.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		log.Fatal(err)
	}
	return f
}

func write(r io.Reader, w io.WriteCloser) error {
	_, err := io.Copy(w, r)
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	return err
}

func uncompress(r *bufio.Reader) error {
	magic, _ := r.Peek(264)

	switch {
	case bytes.HasPrefix(magic, []byte("\x1f\x8b")):
		zr, err := gzip.NewReader(r)
		if err != nil {
			return err
		}
		defer zr.Close()

		if zr.Name != "" {
			targetName = zr.Name
		} else {
			targetName = strings.TrimSuffix(targetName, ".gz")
		}

		return uncompress(bufio.NewReader(zr))

	case bytes.HasPrefix(magic, []byte("BZh")):
		targetName = strings.TrimSuffix(targetName, ".bz2")
		br := bzip2.NewReader(r)
		return uncompress(bufio.NewReader(br))

	case !stdout && bytes.HasPrefix(magic, []byte("PK")):
		return unarchive(zipstream.NewReader(r), target)

	case !stdout && len(magic) > 257 && bytes.HasPrefix(magic[257:], []byte("ustar")):
		return unarchive(tar.NewReader(r), target)

	default:
		return write(r, targetFile())
	}
}

func unarchive(r io.Reader, dir string) error {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	dir += string(filepath.Separator)

	if err := os.MkdirAll(dir, 0777); err != nil {
		return err
	}

	for {
		name, fi, err := unarchiveNext(r)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		path := filepath.Join(dir, filepath.FromSlash(name))
		if !strings.HasPrefix(path, dir) {
			return fmt.Errorf("illegal file path %q", name)
		}

		switch mode := fi.Mode(); {
		case mode.IsDir():
			if err := os.MkdirAll(path, unarchivePerm(mode)); err != nil {
				return err
			}

		case mode.IsRegular():
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
			if err != nil {
				return err
			}

			n, err := io.Copy(f, r)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return fmt.Errorf("error writing to %q: %w", name, err)
			}
			if size := fi.Size(); n != size {
				return fmt.Errorf("wrote %d bytes to %q; expected %d", n, name, size)
			}

			if time := fi.ModTime(); !time.IsZero() {
				_ = os.Chtimes(path, time, time)
			}

		case mode&os.ModeSymlink != 0:
			old, err := ioutil.ReadAll(r)
			if err != nil {
				return err
			}

			err = os.Symlink(string(old), path)
			if err != nil {
				return err
			}

		default:
			return fmt.Errorf("archive contained unsupported file %q of type %v", name, mode)
		}
	}
}

func unarchivePerm(mode os.FileMode) os.FileMode {
	if mode&0007 != 0 {
		mode |= 0001
	}
	if mode&0070 != 0 {
		mode |= 0010
	}
	return mode | 0300
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
		panic(fmt.Sprintf("unarchive: unknown type %T", v))
	}
}
