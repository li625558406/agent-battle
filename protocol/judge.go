package protocol

// TaskManifest 描述一个任务，位于任务目录 task.json。
type TaskManifest struct {
	TaskID      string `json:"task_id"`
	Name        string `json:"name"`
	Description string `json:"description"` // 发给 agent 的任务提示词
	TimeoutSec  int    `json:"timeout_sec"` // agent 执行硬超时，0=默认 600
}

// JudgeManifest 描述判分包，位于任务目录 tests/manifest.json。
type JudgeManifest struct {
	TaskID string        `json:"task_id"`
	Tests  []TestCommand `json:"tests"`
}

type TestCommand struct {
	Name string `json:"name"`
	Cmd  string `json:"cmd"` // 在沙箱 cwd 下用 bash -c 执行
}

type TestResult struct {
	Name     string `json:"name"`
	Passed   bool   `json:"passed"`
	ExitCode int    `json:"exit_code"`
	LogHash  string `json:"log_hash"` // sha256(stdout+stderr)
}

// JudgeReport 是一局判分的最终产物。
type JudgeReport struct {
	TaskID   string       `json:"task_id"`
	Results  []TestResult `json:"results"`
	Passed   int          `json:"passed"`
	Total    int          `json:"total"`
	DiffHash string       `json:"diff_hash"` // sha256(git diff HEAD 输出)
}
