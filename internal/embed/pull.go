package embed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// DefaultStatic is a multilingual (101 languages) model2vec model, 256 dims.
const DefaultStatic = "minishlab/potion-multilingual-128M"

// StaticDir maps a model name to its directory: absolute paths are used as is, a Hugging Face
// repo id or plain name lives under modelsDir by its last path element.
func StaticDir(model, modelsDir string) string {
	if model == "" {
		model = DefaultStatic
	}
	if filepath.IsAbs(model) {
		return model
	}
	return filepath.Join(modelsDir, path.Base(model))
}

var staticFiles = []string{"tokenizer.json", "model.safetensors", "config.json"}

// Pull downloads a model2vec model from Hugging Face into modelsDir, verifying each file
// against the size and LFS sha256 the hub reports. Existing verified files are kept.
func Pull(ctx context.Context, repo, modelsDir string, progress func(string)) (string, error) {
	if repo == "" {
		repo = DefaultStatic
	}
	dir := StaticDir(repo, modelsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	meta, err := hubTree(ctx, repo)
	if err != nil {
		return "", err
	}
	for _, name := range staticFiles {
		m, ok := meta[name]
		if !ok {
			return "", fmt.Errorf("%s has no %s", repo, name)
		}
		dst := filepath.Join(dir, name)
		if verify(dst, m) == nil {
			progress(name + " already present")
			continue
		}
		progress(fmt.Sprintf("downloading %s (%.1f MB)", name, float64(m.Size)/1e6))
		if err := download(ctx, "https://huggingface.co/"+repo+"/resolve/main/"+name, dst+".part"); err != nil {
			return "", err
		}
		if err := verify(dst+".part", m); err != nil {
			os.Remove(dst + ".part")
			return "", fmt.Errorf("%s: %w", name, err)
		}
		if err := os.Rename(dst+".part", dst); err != nil {
			return "", err
		}
	}
	return dir, nil
}

type hubFile struct {
	Size int64
	SHA  string // sha256 of LFS files, empty for small git files
}

func hubTree(ctx context.Context, repo string) (map[string]hubFile, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://huggingface.co/api/models/"+repo+"/tree/main", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hub tree %s: HTTP %d", repo, resp.StatusCode)
	}
	var items []struct {
		Path string `json:"path"`
		Size int64  `json:"size"`
		LFS  *struct {
			Oid  string `json:"oid"`
			Size int64  `json:"size"`
		} `json:"lfs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return nil, err
	}
	out := map[string]hubFile{}
	for _, it := range items {
		f := hubFile{Size: it.Size}
		if it.LFS != nil {
			f.Size, f.SHA = it.LFS.Size, strings.TrimPrefix(it.LFS.Oid, "sha256:")
		}
		out[it.Path] = f
	}
	return out, nil
}

func verify(p string, m hubFile) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() != m.Size {
		return fmt.Errorf("size %d, want %d", st.Size(), m.Size)
	}
	if m.SHA == "" {
		return nil
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != m.SHA {
		return fmt.Errorf("sha256 %s, want %s", got, m.SHA)
	}
	return nil
}

func download(ctx context.Context, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
