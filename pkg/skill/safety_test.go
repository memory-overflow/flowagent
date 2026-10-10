package skill

import (
	"context"
	"strings"
	"testing"

	"github.com/memory-overflow/flowagent/model"
)

// TestDetectDangerousCommand 验证危险删除命令拦截的识别规则。
func TestDetectDangerousCommand(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  bool // true = 应被拦截
	}{
		{"rm -rf 根目录", "rm -rf /", true},
		{"rm -rf 通配", "rm -rf /*", true},
		{"rm -rf 家目录", "rm -rf ~", true},
		{"rm -rf $HOME", "rm -rf $HOME", true},
		{"rm -fr 变体", "rm -fr /tmp/build", true},
		{"rm -Rf 变体", "rm -Rf /tmp/build", true},
		{"rm -r -f 分开", "rm -r -f /tmp/build", true},
		{"rm -f -r 分开", "rm -f -r /tmp/build", true},
		{"rm --recursive --force", "rm --recursive --force /", true},
		{"sudo rm -rf /etc", "sudo rm -rf /etc", true},
		{"多行脚本含 rm -rf", "echo hi\nrm -rf /var/log\n", true},
		{"rm 危险路径单个文件", "rm /etc/passwd", true},
		{"rm 家目录文件", "rm ~/.bashrc", true},
		{"rm /usr 下文件", "rm /usr/local/bin/x", true},

		// 不应拦截的合法场景
		{"rm 普通文件", "rm /tmp/foo.txt", false},
		{"rm 临时目录不带 rf", "rm /tmp/work", false},
		{"rm 相对文件", "rm file.txt", false},
		{"echo 含 rm 字样", "echo 'use rm carefully'", false},
		{"python 调用 pdfkit", "python3 scripts/pdfkit.py extract_text --input a.pdf", false},
		{"rmdir 不应误伤", "rmdir /tmp/empty", false},
		{"空输入", "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := detectDangerousCommand(c.input) != ""
			if got != c.want {
				t.Fatalf("detectDangerousCommand(%q) = %v, want %v (reason=%q)", c.input, got, c.want, detectDangerousCommand(c.input))
			}
		})
	}
}

// TestExecuteBlocksDangerousCommand 验证 Execute 端到端拦截危险命令。
func TestExecuteBlocksDangerousCommand(t *testing.T) {
	cfg := &Config{
		SkillID: "shell",
		Name:    "shell",
		Type:    model.SkillTypeCommand,
		Command: "/bin/sh",
	}

	// 危险命令应被拦截，且不真正执行（返回错误，不返回成功输出）
	out, err := Execute(context.Background(), cfg, "rm -rf /")
	if err == nil {
		t.Fatal("expected error for rm -rf /, got nil")
	}
	if !strings.Contains(err.Error(), "安全策略拦截") {
		t.Fatalf("expected security block message, got: %v", err)
	}
	if out != "" {
		t.Fatalf("expected empty output, got %q", out)
	}

	// 正常命令应照常执行
	out, err = Execute(context.Background(), cfg, "echo safe")
	if err != nil {
		t.Fatalf("normal command should not be blocked: %v", err)
	}
	if out != "safe" {
		t.Fatalf("expected 'safe', got %q", out)
	}
}
