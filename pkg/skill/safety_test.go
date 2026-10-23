package skill

import "testing"

func TestDetectDangerousCommand(t *testing.T) {
	cases := []struct {
		name, input string
		want        bool
	}{
		{"根目录", "rm -rf /", true},
		{"根通配", "rm -fr /*", true},
		{"根隐藏通配", "rm -Rf /.*", true},
		{"家目录", "rm -r -f ~", true},
		{"HOME", "rm --recursive --force $HOME", true},
		{"HOME 花括号", "rm -rf ${HOME}/cache", true},
		{"关键目录", "sudo rm -rf /etc", true},
		{"关键目录后代", "rm /usr/local/bin/x", true},
		{"多行脚本", "echo hi\nrm -rf /var/log\n", true},
		{"双横线后的危险目标", "rm -rf -- /System/Library", true},
		{"等价根路径", "rm -rf /tmp/../", true},
		{"普通目录 fr", "rm -fr /tmp/build", false},
		{"普通目录 Rf", "rm -Rf /tmp/build", false},
		{"普通目录分离参数", "rm -r -f /tmp/build", false},
		{"普通绝对文件", "rm /tmp/foo.txt", false},
		{"普通相对目录", "rm -rf build", false},
		{"echo 含命令字样", "echo 'use rm carefully'", false},
		{"python 调用 pdfkit", "python3 scripts/pdfkit.py extract_text --input a.pdf", false},
		{"rmdir 不误伤", "rmdir /tmp/empty", false},
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

func TestDetectDangerousDeleteTarget(t *testing.T) {
	for _, target := range []string{"/", "//", "/*", "/**", "~/.config", "$HOME/x", "${HOME}/x", "/etc/passwd", "/System/Library"} {
		if got := DetectDangerousDeleteTarget(target); got == "" {
			t.Errorf("目标 %q 应判为危险", target)
		}
	}
	for _, target := range []string{"/tmp/build", "/Users/shared/build", "build", "./output"} {
		if got := DetectDangerousDeleteTarget(target); got != "" {
			t.Errorf("普通目标 %q 不应拦截: %s", target, got)
		}
	}
}
