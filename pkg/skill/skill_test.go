package skill

import "testing"

// TestAbsolutizePaths 验证 skill content 里框架脚本相对路径的绝对化替换。
//
// 背景：pdfkit 等 skill 的 content 写 `skills/xxx/scripts/...`，假设执行
// cwd=项目根；但模型常用 run_command（cwd=任务工作区）落地，相对路径失效。
// 替换成绝对路径后两者皆可用。
func TestAbsolutizePaths(t *testing.T) {
	root := "/srv/flowagent"

	cases := []struct {
		name        string
		in          string
		projectRoot string
		want        string
	}{
		{
			name:        "脚本位于与 setup 两处",
			in:          "脚本位于 skills/pdfkit-py/scripts/。通过 shell 执行 bash skills/pdfkit-py/scripts/setup.sh",
			projectRoot: root,
			want:        "脚本位于 /srv/flowagent/skills/pdfkit-py/scripts/。通过 shell 执行 bash /srv/flowagent/skills/pdfkit-py/scripts/setup.sh",
		},
		{
			name:        "CLI 完整路径多处替换",
			in:          "skills/pdfkit-py/scripts/venv/bin/python3 skills/pdfkit-py/scripts/pdfkit.py",
			projectRoot: root,
			want:        "/srv/flowagent/skills/pdfkit-py/scripts/venv/bin/python3 /srv/flowagent/skills/pdfkit-py/scripts/pdfkit.py",
		},
		{
			name:        "空 content 原样返回",
			in:          "",
			projectRoot: root,
			want:        "",
		},
		{
			name:        "空 projectRoot 原样返回",
			in:          "skills/a/b",
			projectRoot: "",
			want:        "skills/a/b",
		},
		{
			name:        "不误伤裸 skills 字样",
			in:          "使用 skills 技能完成 PDF 处理",
			projectRoot: root,
			want:        "使用 skills 技能完成 PDF 处理",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := AbsolutizePaths(c.in, c.projectRoot)
			if got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}
