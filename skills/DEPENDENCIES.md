# Skills 部署依赖清单

脚本型 skill 需要在 **flowagent 部署服务器**上安装对应运行时依赖后，才能被内置的 `run_command` 工具真正执行。本文件是部署时的安装清单。

## 一键安装（首次部署）

```bash
bash skills/setup.sh           # 仅核心依赖（推荐首次）
bash skills/setup.sh --all     # 连可选依赖一起装（表格提取/公式识别/pandas 等，体积大）
```

脚本会依次：检测环境（python3/uv）→ 装 pdfkit-py 依赖 → 校验 minimax-xlsx 脚本 → 用 uv 装 browser-use CLI → 检测外部工具（gs/tesseract/soffice）→ 汇总验证。幂等可重复执行。下面各节是脚本背后的明细，便于手工安装或排查。

## 目录结构

```
skills/
├── builtins/          内置清单 JSON（一键导入 DB 的指令模板）
├── pdfkit-py/         PDF 处理脚本（scripts/pdfkit.py + 50 个命令模块）
├── minimax-xlsx/      Excel 处理脚本（scripts/*.py + templates/minimal_xlsx/）
├── browser-use/       浏览器自动化（无本地脚本，依赖第三方 CLI）
├── setup.sh           首次安装总入口（一键装 3 个脚本型 skill 依赖）
└── DEPENDENCIES.md    本清单
```

## 执行链路说明

skill 在框架内只是注入 system prompt 的指令（不区分类型、不注册为工具），真正的执行由内置 **run_command** 工具落地：

```
模型读到 skill 指令 → 调用 run_command 执行命令 → sh -c（Windows: cmd /c）运行 → stdout 回传
```

skill 指令里 `skills/<skill>/scripts/` 形式的相对路径在注入前会被替换为项目根下的绝对路径，因此与 run_command 的工作目录（任务工作区）无关。

---

## 1. pdfkit-py — PDF 全能工具箱

- **脚本位置**：`skills/pdfkit-py/scripts/pdfkit.py`
- **运行时**：Python 3.10+（`setup.sh` 会自动检测；缺失则下载独立 Python 3.13 运行时，无需管理员权限）

### 一键初始化

```bash
bash skills/pdfkit-py/scripts/setup.sh           # 仅核心依赖（约 15s）
bash skills/pdfkit-py/scripts/setup.sh --all     # 含全部可选依赖（约 2min）
```

### 核心依赖（由 `requirements.txt` 自动安装）

`PyMuPDF`、`pypdf`、`Pillow`、`reportlab`、`numpy`、`pdfplumber`、`pytesseract`、`markdown`、`python-docx`、`fonttools`

### 可选依赖（按需，命令报 `ModuleNotFoundError` 时用 venv pip 安装）

| 包 | 涉及命令 |
|---|---|
| `camelot-py[cv]` | extract_table（lattice/stream） |
| `tabula-py` | extract_table（tabula） |
| `pdf2docx` | pdf_to_word |
| `pikepdf` | compress（高级压缩） |
| `pix2tex` | formula_detect（model） |

### 外部系统工具（可选，按需）

| 工具 | 用途 | macOS | Linux |
|---|---|---|---|
| `gs` (Ghostscript) | PDF 压缩 | `brew install ghostscript` | `sudo apt install -y ghostscript` |
| `tesseract` | OCR | `brew install tesseract` | `sudo apt install -y tesseract-ocr` |
| `tesseract-lang` | OCR 中文 | `brew install tesseract-lang` | `sudo apt install -y tesseract-ocr-chi-sim` |
| `soffice` (LibreOffice) | 格式转换 | `brew install --cask libreoffice` | `sudo apt install -y libreoffice` |

### 验证

```bash
skills/pdfkit-py/scripts/venv/bin/python3 skills/pdfkit-py/scripts/pdfkit.py help
```

---

## 2. minimax-xlsx — Excel 表格处理

- **脚本位置**：`skills/minimax-xlsx/scripts/*.py` + `templates/minimal_xlsx/`
- **运行时**：Python 3（核心脚本**只用标准库，零第三方依赖**）

### 核心脚本（纯标准库，开箱即用）

`xlsx_reader.py`、`xlsx_unpack.py`、`xlsx_pack.py`、`formula_check.py`、`xlsx_add_column.py`、`xlsx_insert_row.py`、`xlsx_shift_rows.py`、`shared_strings_builder.py`、`style_audit.py`

### 可选依赖（按需）

| 依赖 | 用途 | 安装 |
|---|---|---|
| `pandas` + `openpyxl` | READ 数据分析 | `pip install pandas openpyxl` |
| `libreoffice` | 动态重算（`libreoffice_recalc.py`） | `sudo apt install -y libreoffice` |

### 验证

```bash
python3 skills/minimax-xlsx/scripts/formula_check.py --help
```

---

## 3. browser-use — 浏览器自动化

- **脚本位置**：无本地脚本，依赖第三方 `browser-use` CLI（v3.0 / Browser Harness）
- **运行时**：Python 3.12（由 uv 自动下载独立解释器，无需管理员权限）

### 一键安装（官方推荐，用 uv）

```bash
uv tool install browser-use          # 默认拉取 Python 3.12
# 或显式指定解释器：
uv tool install --python 3.12 browser-use
```

> ⚠️ 旧版 `curl -fsSL https://browser-use.com/cli/install.sh | bash` 已失效（404），请勿使用。

### 验证安装

```bash
browser-use --doctor     # 应看到 chrome running 为 ok
browser-use --version    # 0.1.9 (pypi)
```

### 说明

- **接口为 v3.0**：不再有 `open/state/click` 等子命令，改为从 stdin 传 Python（helpers 预导入），例如：
  ```bash
  browser-use <<'PY'
  new_tab("https://example.com")
  print(page_info())
  PY
  ```
- 本地模式默认连接正在运行的 Chrome/Chromium（CDP）；macOS 弹出「允许远程调试」时执行 `browser-use mac-approve`。
- 云浏览器（无头/并行场景）需 `browser-use auth login` 登录，再 `start_remote_daemon("name")` 启动。
