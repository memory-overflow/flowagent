package skill

import (
	"os"
	"path/filepath"
	"testing"
)

// venvBinDirFor 的跨平台约束（skill 包）：
//   - unix    → <root>/.venv/bin
//   - windows → <root>/.venv/Scripts
//   - 目录不存在 / projectRoot 为空 → 空串（调用方跳过 PATH 注入）。
//
// GOOS 走参数注入，任意平台都能覆盖两个分支。
func TestVenvBinDirFor(t *testing.T) {
	t.Run("unix layout", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, ".venv", "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		if got := venvBinDirFor("darwin", root); got != filepath.Join(root, ".venv", "bin") {
			t.Errorf("unix 分支应返回 .venv/bin，got %q", got)
		}
	})

	t.Run("windows layout", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, ".venv", "Scripts"), 0o755); err != nil {
			t.Fatal(err)
		}
		if got := venvBinDirFor("windows", root); got != filepath.Join(root, ".venv", "Scripts") {
			t.Errorf("windows 分支应返回 .venv/Scripts，got %q", got)
		}
	})

	t.Run("missing venv", func(t *testing.T) {
		if got := venvBinDirFor("windows", t.TempDir()); got != "" {
			t.Errorf("venv 不存在应返回空串，got %q", got)
		}
		if got := venvBinDirFor("windows", ""); got != "" {
			t.Errorf("projectRoot 为空应返回空串，got %q", got)
		}
	})
}
