#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""pptx.py — PPT 演示文稿（.pptx）核心处理工具箱统一入口。

用法：
    python3 pptx_tool.py extract_text --input a.pptx [--json]
    python3 pptx_tool.py info --input a.pptx
    python3 pptx_tool.py create --output a.pptx --markdown outline.md
    python3 pptx_tool.py create --output a.pptx --content "# 主题\\n## 小节\\n- 要点"

依赖：python-pptx（pip install python-pptx）。
输出：命令结果统一为 JSON（{"ok": true, "data": ...} / {"ok": false, "error": "..."}）。
"""

import sys
import json
import argparse

try:
    from pptx import Presentation
    from pptx.util import Pt
except ImportError:
    print(json.dumps({"ok": False, "error": "缺少 python-pptx 依赖，请先执行 pip install python-pptx"}), file=sys.stderr)
    sys.exit(1)


def _ok(data):
    print(json.dumps({"ok": True, "data": data}, ensure_ascii=False))


def _fail(msg):
    print(json.dumps({"ok": False, "error": msg}, ensure_ascii=False), file=sys.stderr)
    sys.exit(1)


def _slide_title(slide):
    if slide.shapes.title is not None:
        return slide.shapes.title.text
    return ""


def _slide_body_texts(slide):
    """收集非标题占位符/文本框的文本，按形状顺序。"""
    texts = []
    for shape in slide.shapes:
        if not shape.has_text_frame:
            continue
        # 跳过标题占位符（已单独取）
        if shape == slide.shapes.title:
            continue
        for para in shape.text_frame.paragraphs:
            t = ''.join(run.text for run in para.runs).strip()
            if t:
                texts.append(t)
    return texts


def cmd_extract_text(args):
    prs = Presentation(args.input)
    slides = []
    for i, slide in enumerate(prs.slides, 1):
        title = _slide_title(slide)
        bodies = _slide_body_texts(slide)
        slides.append({"index": i, "title": title, "body": bodies})
    if getattr(args, 'json', False):
        _ok({"slides": slides, "count": len(slides)})
    else:
        for s in slides:
            print(f"## 第 {s['index']} 页：{s['title'] or '(无标题)'}")
            for b in s['body']:
                print(f"- {b}")
            print()


def cmd_info(args):
    prs = Presentation(args.input)
    slides = []
    for i, slide in enumerate(prs.slides, 1):
        bodies = _slide_body_texts(slide)
        text_len = sum(len(b) for b in bodies) + len(_slide_title(slide))
        slides.append({
            "index": i,
            "title": _slide_title(slide),
            "body_items": len(bodies),
            "text_chars": text_len,
        })
    _ok({"slide_count": len(prs.slides), "slides": slides})


def _parse_outline(text):
    """把 markdown 大纲解析成 slides：[(title, [bullets])]。"""
    slides = []
    cur = None
    for line in text.split('\n'):
        s = line.rstrip()
        if not s.strip():
            continue
        if s.startswith('# ') or s.startswith('## '):
            title = s.lstrip('#').strip()
            cur = [title, []]
            slides.append(cur)
        elif s.lstrip().startswith('- ') or s.lstrip().startswith('* '):
            bullet = s.lstrip()[2:].strip()
            if cur is not None:
                cur[1].append(bullet)
            elif slides:
                slides[-1][1].append(bullet)
        else:
            if cur is not None:
                cur[1].append(s.strip())
            elif slides:
                slides[-1][1].append(s.strip())
    return [s for s in slides if s[0] or s[1]]


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

    slides = _parse_outline(text)
    if not slides:
        _fail("大纲为空，无法创建演示文稿")

    prs = Presentation()
    # 标题页（第一张）用标题版式
    first_title, first_bullets = slides[0]
    title_layout = prs.slide_layouts[0]
    title_slide = prs.slides.add_slide(title_layout)
    title_slide.shapes.title.text = first_title
    if first_bullets:
        sub = title_slide.placeholders[1]
        sub.text = '\n'.join(first_bullets)

    # 后续每段用标题+内容版式
    content_layout = prs.slide_layouts[1]
    for title, bullets in slides[1:]:
        slide = prs.slides.add_slide(content_layout)
        slide.shapes.title.text = title
        if bullets:
            body = slide.placeholders[1].text_frame
            body.text = bullets[0]
            for b in bullets[1:]:
                p = body.add_paragraph()
                p.text = b
                p.level = 0

    prs.save(args.output)
    _ok({"output": args.output, "slides": len(prs.slides)})


def main():
    p = argparse.ArgumentParser(description="PPT 演示文稿核心处理工具")
    sub = p.add_subparsers(dest='cmd', required=True)

    e = sub.add_parser('extract_text', help='提取全文（每页标题+正文）')
    e.add_argument('--input', required=True)
    e.add_argument('--json', action='store_true')
    e.set_defaults(fn=cmd_extract_text)

    i = sub.add_parser('info', help='结构发现（页数/标题/文本量）')
    i.add_argument('--input', required=True)
    i.set_defaults(fn=cmd_info)

    c = sub.add_parser('create', help='从 markdown 大纲创建演示文稿')
    c.add_argument('--output', required=True)
    c.add_argument('--markdown', help='markdown 大纲文件路径')
    c.add_argument('--content', help='markdown 大纲内容字符串')
    c.set_defaults(fn=cmd_create)

    args = p.parse_args()
    try:
        args.fn(args)
    except Exception as e:
        _fail(str(e))


if __name__ == '__main__':
    main()
