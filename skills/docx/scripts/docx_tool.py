#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""docx.py — Word 文档（.docx）核心处理工具箱统一入口。

用法：
    python3 docx_tool.py extract_text --input a.docx [--json]
    python3 docx_tool.py info --input a.docx
    python3 docx_tool.py create --output a.docx --markdown content.md
    python3 docx_tool.py create --output a.docx --content "# 标题\\n正文"
    python3 docx_tool.py append_text --input a.docx --output b.docx --content "追加段落"

依赖：python-docx（pip install python-docx）。
输出：命令结果统一为 JSON（{"ok": true, "data": ...} / {"ok": false, "error": "..."}）。
"""

import sys
import os
import json
import argparse

try:
    import docx
    from docx import Document
    from docx.shared import Pt
except ImportError:
    print(json.dumps({"ok": False, "error": "缺少 python-docx 依赖，请先执行 pip install python-docx"}), file=sys.stderr)
    sys.exit(1)


def _ok(data):
    print(json.dumps({"ok": True, "data": data}, ensure_ascii=False))


def _fail(msg):
    print(json.dumps({"ok": False, "error": msg}, ensure_ascii=False), file=sys.stderr)
    sys.exit(1)


def _body_blocks(doc):
    """按文档顺序遍历段落与表格，返回 [(kind, obj)]，kind ∈ {p, tbl}。"""
    from docx.oxml.ns import qn
    blocks = []
    for child in doc.element.body.iterchildren():
        if child.tag == qn('w:p'):
            # 用段落 id 反查 docx 段落对象
            for p in doc.paragraphs:
                if p._element is child:
                    blocks.append(('p', p))
                    break
        elif child.tag == qn('w:tbl'):
            for t in doc.tables:
                if t._element is child:
                    blocks.append(('tbl', t))
                    break
    return blocks


def _para_text(p):
    return p.text


def _heading_level(p):
    style = (p.style.name or '').lower()
    if style.startswith('heading'):
        tail = style.replace('heading', '').strip()
        if tail.isdigit():
            return int(tail)
    if style == 'title':
        return 1
    return 0


def cmd_extract_text(args):
    doc = Document(args.input)
    lines = []
    for kind, obj in _body_blocks(doc):
        if kind == 'p':
            txt = _para_text(obj)
            if not txt.strip():
                continue
            lvl = _heading_level(obj)
            if lvl:
                lines.append('#' * lvl + ' ' + txt)
            elif obj.style.name.lower() in ('list bullet', 'list number'):
                lines.append('- ' + txt)
            else:
                lines.append(txt)
        else:
            for row in obj.rows:
                cells = [c.text.strip().replace('\n', ' ') for c in row.cells]
                lines.append('| ' + ' | '.join(cells) + ' |')
            lines.append('')  # 表格后空行
    text = '\n'.join(lines).strip()
    if getattr(args, 'json', False):
        _ok({"text": text, "chars": len(text)})
    else:
        print(text)


def cmd_info(args):
    doc = Document(args.input)
    paras = doc.paragraphs
    headings = [{"level": _heading_level(p), "text": p.text} for p in paras if _heading_level(p)]
    tables = [{"index": i, "rows": len(t.rows), "cols": len(t.columns)} for i, t in enumerate(doc.tables)]
    sections = len(doc.sections)
    nonempty = [p for p in paras if p.text.strip()]
    _ok({
        "paragraphs": len(paras),
        "nonempty_paragraphs": len(nonempty),
        "headings": headings,
        "tables": tables,
        "sections": sections,
    })


def _markdown_to_blocks(text):
    """把 markdown 文本拆成 (kind, payload) 序列，kind ∈ heading/para/list/table。"""
    blocks = []
    lines = text.split('\n')
    i = 0
    while i < len(lines):
        line = lines[i].rstrip()
        if not line.strip():
            i += 1
            continue
        # 表格：连续的 | 开头的行
        if line.strip().startswith('|'):
            tbl = []
            while i < len(lines) and lines[i].strip().startswith('|'):
                row = [c.strip() for c in lines[i].strip().strip('|').split('|')]
                if not all(v in ('---', ':-', '-:', ':-:', '') for v in row if v):
                    tbl.append(row)
                i += 1
            if tbl:
                blocks.append(('table', tbl))
            continue
        # 标题
        if line.startswith('#'):
            n = len(line) - len(line.lstrip('#'))
            blocks.append(('heading', (n, line.lstrip('#').strip())))
            i += 1
            continue
        # 列表
        if line.lstrip().startswith(('- ', '* ', '+ ')) or (len(line) >= 2 and line[0].isdigit() and line[1] in '.、)'):
            items = []
            while i < len(lines):
                ln = lines[i].strip()
                if ln.startswith(('- ', '* ', '+ ')):
                    items.append(ln[2:].strip())
                    i += 1
                elif len(ln) >= 2 and ln[0].isdigit() and ln[1] in '.、)':
                    items.append(ln[2:].strip())
                    i += 1
                else:
                    break
            blocks.append(('list', items))
            continue
        # 普通段落
        blocks.append(('para', line.strip()))
        i += 1
    return blocks


def cmd_create(args):
    if args.markdown:
        try:
            with open(args.markdown, 'r', encoding='utf-8') as f:
                text = f.read()
        except OSError as e:
            _fail(f"读取 markdown 文件失败: {e}")
    elif args.content:
        text = args.content
    else:
        _fail("create 需要 --markdown 或 --content 提供内容")

    doc = Document()
    for kind, payload in _markdown_to_blocks(text):
        if kind == 'heading':
            lvl, txt = payload
            doc.add_heading(txt, level=min(lvl, 4))
        elif kind == 'para':
            doc.add_paragraph(payload)
        elif kind == 'list':
            for item in payload:
                doc.add_paragraph(item, style='List Bullet')
        elif kind == 'table':
            rows = max(1, len(payload))
            cols = max(1, len(payload[0]))
            t = doc.add_table(rows=rows, cols=cols)
            t.style = 'Table Grid'
            for ri, row in enumerate(payload):
                for ci in range(cols):
                    t.cell(ri, ci).text = row[ci] if ci < len(row) else ''
    doc.save(args.output)
    _ok({"output": args.output, "paragraphs": len(doc.paragraphs), "tables": len(doc.tables)})


def cmd_append_text(args):
    doc = Document(args.input)
    for kind, payload in _markdown_to_blocks(args.content):
        if kind == 'heading':
            lvl, txt = payload
            doc.add_heading(txt, level=min(lvl, 4))
        elif kind == 'list':
            for item in payload:
                doc.add_paragraph(item, style='List Bullet')
        elif kind == 'table':
            rows = max(1, len(payload))
            cols = max(1, len(payload[0]))
            t = doc.add_table(rows=rows, cols=cols)
            t.style = 'Table Grid'
            for ri, row in enumerate(payload):
                for ci in range(cols):
                    t.cell(ri, ci).text = row[ci] if ci < len(row) else ''
        else:
            doc.add_paragraph(payload)
    doc.save(args.output)
    _ok({"output": args.output})


def main():
    p = argparse.ArgumentParser(description="Word 文档核心处理工具")
    sub = p.add_subparsers(dest='cmd', required=True)

    e = sub.add_parser('extract_text', help='提取全文（段落+表格，markdown 近似）')
    e.add_argument('--input', required=True)
    e.add_argument('--json', action='store_true')
    e.set_defaults(fn=cmd_extract_text)

    i = sub.add_parser('info', help='结构发现（标题/段落/表格/节）')
    i.add_argument('--input', required=True)
    i.set_defaults(fn=cmd_info)

    c = sub.add_parser('create', help='从 markdown 创建文档')
    c.add_argument('--output', required=True)
    c.add_argument('--markdown', help='markdown 源文件路径')
    c.add_argument('--content', help='markdown 内容字符串')
    c.set_defaults(fn=cmd_create)

    a = sub.add_parser('append_text', help='追加内容到现有文档')
    a.add_argument('--input', required=True)
    a.add_argument('--output', required=True)
    a.add_argument('--content', required=True)
    a.set_defaults(fn=cmd_append_text)

    args = p.parse_args()
    try:
        args.fn(args)
    except Exception as e:
        _fail(str(e))


if __name__ == '__main__':
    main()
