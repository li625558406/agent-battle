package judge

import (
	"os"
	"path/filepath"
	"testing"
)

const devKey = "dev-secret" // M1 开发密钥；平台化后由平台签发

func setupTask(t *testing.T) (taskDir, sandbox string) {
	t.Helper()
	taskDir = t.TempDir()
	testsDir := filepath.Join(taskDir, "tests")
	if err := os.MkdirAll(testsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := `{"task_id":"t1","tests":[{"name":"ok","cmd":"bash .judge/run.sh"},{"name":"bad","cmd":"bash .judge/fail.sh"}]}`
	if err := os.WriteFile(filepath.Join(testsDir, "manifest.json"), []byte(m), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(testsDir, "run.sh"), []byte("exit 0"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(testsDir, "fail.sh"), []byte("exit 1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SignDir(taskDir, []byte(devKey)); err != nil {
		t.Fatal(err)
	}

	sandbox = t.TempDir()
	if err := os.WriteFile(filepath.Join(sandbox, "work.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return taskDir, sandbox
}

func TestRunVerifiesAndScores(t *testing.T) {
	taskDir, sb := setupTask(t)
	rep, err := Run(taskDir, sb, []byte(devKey))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total != 2 || rep.Passed != 1 {
		t.Fatalf("want 1/2, got %d/%d", rep.Passed, rep.Total)
	}
	if rep.Results[0].Name != "ok" || !rep.Results[0].Passed {
		t.Fatalf("bad first result: %+v", rep.Results[0])
	}
	if rep.DiffHash == "" {
		t.Fatal("diff hash empty")
	}
}

func TestTamperedManifestRejected(t *testing.T) {
	taskDir, sb := setupTask(t)
	mp := filepath.Join(taskDir, "tests", "manifest.json")
	if err := os.WriteFile(mp, []byte(`{"task_id":"t1","tests":[{"name":"evil","cmd":"echo pwned"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(taskDir, sb, []byte(devKey)); err == nil {
		t.Fatal("tampered judge package accepted")
	}
}

func TestWrongKeyRejected(t *testing.T) {
	taskDir, sb := setupTask(t)
	if _, err := Run(taskDir, sb, []byte("wrong-key")); err == nil {
		t.Fatal("wrong key accepted")
	}
}
