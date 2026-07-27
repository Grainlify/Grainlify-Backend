package docs_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// linkPattern matches markdown links [text](path) where path is a relative .md link.
var linkPattern = regexp.MustCompile(`\[[^\]]*\]\(([^)#\s]+\.md[^)]*)\)`)

func TestMarkdownInternalLinksResolve(t *testing.T) {
	t.Helper()
	root := findDocsRoot(t)

	var files []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(path, ".md") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk docs: %v", err)
	}

	repoRoot := filepath.Dir(root)
	readmePath := filepath.Join(repoRoot, "README.md")
	files = append(files, readmePath)

	for _, file := range files {
		content, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		baseDir := filepath.Dir(file)
		for _, match := range linkPattern.FindAllStringSubmatch(string(content), -1) {
			target := strings.Split(match[1], "#")[0]
			if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
				continue
			}
			resolved := filepath.Clean(filepath.Join(baseDir, target))
			if _, err := os.Stat(resolved); err != nil {
				rel, _ := filepath.Rel(repoRoot, file)
				t.Errorf("%s: broken link %q -> %s", rel, match[1], target)
			}
		}
	}
}

func findDocsRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		candidate := filepath.Join(dir, "docs", "README.md")
		if _, err := os.Stat(candidate); err == nil {
			return filepath.Join(dir, "docs")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find docs/README.md from working directory")
		}
		dir = parent
	}
}
