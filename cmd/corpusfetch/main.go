package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const maxArchiveBytes = 128 << 20
const maxCorpusBytes = 128 << 20

type lockFile struct {
	Schema  int      `json:"schema"`
	Corpora []corpus `json:"corpora"`
}

type corpus struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Tier        string `json:"tier"`
	URL         string `json:"url"`
	SHA256      string `json:"sha256"`
	WasmSHA256  string `json:"wasm_sha256"`
	Member      string `json:"member"`
	Output      string `json:"output"`
	Expectation string `json:"expectation"`
}

func main() {
	full := flag.Bool("full", false, "fetch the full corpus instead of the quick CI subset")
	lockPath := flag.String("lock", "corpus.lock.json", "corpus lock file")
	outputDir := flag.String("dir", ".corpus", "output directory")
	flag.Parse()
	if err := run(*lockPath, *outputDir, *full); err != nil {
		fmt.Fprintln(os.Stderr, "corpusfetch:", err)
		os.Exit(1)
	}
}

func run(lockPath, outputDir string, full bool) error {
	contents, err := os.ReadFile(lockPath)
	if err != nil {
		return err
	}
	var lock lockFile
	if err := json.Unmarshal(contents, &lock); err != nil {
		return fmt.Errorf("decode lock: %w", err)
	}
	if lock.Schema != 1 {
		return fmt.Errorf("unsupported lock schema %d", lock.Schema)
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return err
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	for _, item := range lock.Corpora {
		if !full && item.Tier != "quick" {
			continue
		}
		if filepath.Base(item.Output) != item.Output || item.Output == "." {
			return fmt.Errorf("%s: unsafe output name %q", item.Name, item.Output)
		}
		path := filepath.Join(outputDir, item.Output)
		if digestMatches(path, item.WasmSHA256) {
			fmt.Printf("cached  %-24s %s\n", item.Name, path)
			continue
		}
		fmt.Printf("fetch   %-24s %s@%s (%s)\n", item.Name, item.Name, item.Version, item.Expectation)
		if err := fetchOne(client, outputDir, path, item); err != nil {
			return fmt.Errorf("%s: %w", item.Name, err)
		}
	}
	return nil
}

func digestMatches(path, wanted string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return false
	}
	return strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), wanted)
}

func fetchOne(client *http.Client, dir, outputPath string, item corpus) error {
	request, err := http.NewRequest(http.MethodGet, item.URL, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned %s", response.Status)
	}
	archive, err := os.CreateTemp(dir, ".archive-*")
	if err != nil {
		return err
	}
	archivePath := archive.Name()
	defer os.Remove(archivePath)
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(archive, hash), io.LimitReader(response.Body, maxArchiveBytes+1))
	closeErr := archive.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if written > maxArchiveBytes {
		return fmt.Errorf("archive exceeds %d bytes", maxArchiveBytes)
	}
	gotDigest := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(gotDigest, item.SHA256) {
		return fmt.Errorf("archive sha256 = %s, want %s", gotDigest, item.SHA256)
	}
	return extractMember(archivePath, outputPath, item.Member, item.WasmSHA256)
}

func extractMember(archivePath, outputPath, member, wantedDigest string) error {
	archive, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer archive.Close()
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("archive member %q not found", member)
		}
		if err != nil {
			return err
		}
		if header.Name != member {
			continue
		}
		if header.Typeflag != tar.TypeReg || header.Size < 8 || header.Size > maxCorpusBytes {
			return fmt.Errorf("archive member %q is not a bounded regular file", member)
		}
		output, err := os.CreateTemp(filepath.Dir(outputPath), ".corpus-*")
		if err != nil {
			return err
		}
		tempPath := output.Name()
		defer os.Remove(tempPath)
		hash := sha256.New()
		written, copyErr := io.CopyN(io.MultiWriter(output, hash), tr, header.Size)
		closeErr := output.Close()
		if copyErr != nil || written != header.Size {
			return fmt.Errorf("extract %q: %w", member, copyErr)
		}
		if closeErr != nil {
			return closeErr
		}
		gotDigest := hex.EncodeToString(hash.Sum(nil))
		if !strings.EqualFold(gotDigest, wantedDigest) {
			return fmt.Errorf("archive member sha256 = %s, want %s", gotDigest, wantedDigest)
		}
		file, err := os.Open(tempPath)
		if err != nil {
			return err
		}
		var prefix [4]byte
		_, readErr := io.ReadFull(file, prefix[:])
		closeErr = file.Close()
		if readErr != nil || closeErr != nil || string(prefix[:]) != "\x00asm" {
			return fmt.Errorf("archive member %q is not WebAssembly", member)
		}
		if err := os.Chmod(tempPath, 0o644); err != nil {
			return err
		}
		return os.Rename(tempPath, outputPath)
	}
}
