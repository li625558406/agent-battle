package sandbox

import (
	"os"
	"path/filepath"
	"sync"
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

// C1：seed 嵌套目录内容必须被拷入，且嵌套的伪 .git 目录必须被跳过。
func TestCreateCopiesNestedSeed(t *testing.T) {
	seed := t.TempDir()
	write(t, seed, "src/nested/util.go", "package util\n")
	write(t, seed, "sub/.git/Foo", "fake repo deep inside")

	sb, cleanup, err := Create(seed)
	defer cleanup()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(sb, "src", "nested", "util.go"))
	if err != nil {
		t.Fatalf("nested seed file not copied: %v", err)
	}
	if string(data) != "package util\n" {
		t.Fatalf("nested file content mismatch: %q", data)
	}
	if _, err := os.Stat(filepath.Join(sb, "sub", ".git")); !os.IsNotExist(err) {
		t.Fatalf("nested .git must be skipped, got stat err: %v", err)
	}
}

// C2：并发调用 cleanup 不得有数据竞争（-race 验证），且目录只删一次。
func TestCleanupConcurrent(t *testing.T) {
	seed := t.TempDir()
	write(t, seed, "a.txt", "hi")
	sb, cleanup, err := Create(seed)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cleanup()
		}()
	}
	wg.Wait()
	if _, err := os.Stat(sb); !os.IsNotExist(err) {
		t.Fatal("sandbox not removed after concurrent cleanup")
	}
}
