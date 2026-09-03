package emscripten

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type corpusManifest struct {
	Schema  int `json:"schema"`
	Corpora []struct {
		Name        string `json:"name"`
		Tier        string `json:"tier"`
		SHA256      string `json:"sha256"`
		WasmSHA256  string `json:"wasm_sha256"`
		Member      string `json:"member"`
		Output      string `json:"output"`
		Expectation string `json:"expectation"`
	} `json:"corpora"`
}

func TestCorpusManifestAndDownloadedBoundaries(t *testing.T) {
	contents, err := os.ReadFile("corpus.lock.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest corpusManifest
	if err := json.Unmarshal(contents, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Schema != 1 || len(manifest.Corpora) < 8 {
		t.Fatalf("corpus manifest schema=%d entries=%d", manifest.Schema, len(manifest.Corpora))
	}
	seen := make(map[string]bool)
	for _, item := range manifest.Corpora {
		t.Run(item.Name, func(t *testing.T) {
			if seen[item.Output] {
				t.Fatalf("duplicate output %q", item.Output)
			}
			seen[item.Output] = true
			if item.Tier != "quick" && item.Tier != "full" {
				t.Fatalf("invalid tier %q", item.Tier)
			}
			for name, digest := range map[string]string{"archive": item.SHA256, "wasm": item.WasmSHA256} {
				decoded, err := hex.DecodeString(digest)
				if err != nil || len(decoded) != sha256.Size {
					t.Fatalf("invalid %s digest %q", name, digest)
				}
			}
			path := filepath.Join(".corpus", item.Output)
			source, err := os.ReadFile(path)
			if os.IsNotExist(err) {
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			gotDigest := sha256.Sum256(source)
			if hex.EncodeToString(gotDigest[:]) != item.WasmSHA256 {
				t.Fatal("downloaded corpus digest does not match lock")
			}
			if item.Expectation != "executes" && item.Expectation != "wago-amd64-gap" {
				transformed, err := transformModule(source, nil)
				if err != nil {
					t.Fatalf("non-standalone corpus should remain untouched, got %v", err)
				}
				if !bytes.Equal(transformed, source) {
					t.Fatal("non-standalone corpus was transformed")
				}
			}
		})
	}
}
