package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCreateCopiesSeedAndInitsGit(t *testing.T) {
	seed := t.TempDir()
	write(t, seed, "calc.sh", "add() { echo $((a-b)); }\n")
	write(t, seed, ".git", "not-a-repo") // seed 里的 .git 必须被跳过

	sb, cleanup, err := Create(seed)
	defer cleanup()
	if err != nil {
		t.Fatal(err)
	}
	if sb == "" || sb == seed {
		t.Fatalf("sandbox path invalid: %q", sb)
	}
	if _, err := os.Stat(filepath.Join(sb, "calc.sh")); err != nil {
		t.Fatalf("seed file not copied: %v", err)
	}
	// 原断言 "sb/.git 不存在" 与下一条 ".git/HEAD 必须存在" 矛盾（git init 必建 .git）。
	// 修正为：.git 必须是 git init 产生的目录，而不是 seed 拷来的普通文件。
	if fi, err := os.Stat(filepath.Join(sb, ".git")); err != nil || !fi.IsDir() {
		t.Fatalf("sandbox .git should be a fresh git dir, not seed's file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sb, ".git", "HEAD")); err != nil {
		t.Fatalf("git init missing HEAD: %v", err)
	}
}

func TestCleanupRemovesSandbox(t *testing.T) {
	seed := t.TempDir()
	write(t, seed, "a.txt", "hi")
	sb, cleanup, err := Create(seed)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if _, err := os.Stat(sb); !os.IsNotExist(err) {
		t.Fatal("sandbox not removed after cleanup")
	}
}
