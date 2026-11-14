#!/usr/bin/env python3
# @author: kevinkelly
# @license: Apache Licence
# @file: build_help.py
# @time: 2026/10/28

"""打包使用手册。

help/*.md 是手册的唯一来源。本脚本：
  1. 从 js/perm.js 解析 PERM_TREE（模块 -> 页面 -> 按钮码），生成 help/permcodes.md（权限码清单，避免手工维护不一致）；
  2. 校验章节：perm.js 的 NAV_TREE 里每个 help 字段都有对应章节、站内链接 #help/xxx 都指向存在的章节；
  3. 把全部章节打包为 js/help_content.js（window.HELP_MD），使 index.html 以 file:// 打开也能阅读手册；
  4. 合并为 docs/用户使用手册.md（可单独分发 / 转 Word、PDF）。

用法（在 prototype 目录下）：python tools/build_help.py
"""
import json
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
HELP = ROOT / 'help'
ORDER = ['quickstart', 'login', 'sqlworkflow', 'optimize', 'query', 'slowlog', 'partman', 'archive', 'export', 'dict',
         'qpriv', 'perf', 'instance', 'inspect', 'sysadmin', 'btnperm', 'permcodes', 'faq']
KIND = {'v': '查看', 'o': '操作', 'd': '高危'}


def parse_perm_tree(src):
    """逐个 token 扫描 PERM_TREE，返回 [{name, pages: [{key, name, btns: [(code, label, kind, locked)]}]}]。"""
    start = src.index('const PERM_TREE')
    end = src.index('/* 扁平索引', start)
    body = src[start:end]
    tok = re.compile(
        r"\{name: '(?P<mod>[^']+)', pages: \["
        r"|\{key: '(?P<pkey>[\w]+)', name: '(?P<pname>[^']+)', btns: \["
        r"|\['(?P<code>[\w.]+)', '(?P<label>[^']+)', '(?P<kind>[vod])'(?:, '(?P<lock>L)')?\]")
    tree, mod, page = [], None, None
    for m in tok.finditer(body):
        if m.group('mod'):
            mod = {'name': m.group('mod'), 'pages': []}
            tree.append(mod)
        elif m.group('pkey'):
            if mod is None:
                raise SystemExit('perm.js 解析失败：页面出现在模块之前')
            page = {'key': m.group('pkey'), 'name': m.group('pname'), 'btns': []}
            mod['pages'].append(page)
        else:
            if page is None:
                raise SystemExit('perm.js 解析失败：按钮出现在页面之前')
            page['btns'].append((m.group('code'), m.group('label'), m.group('kind'), bool(m.group('lock'))))
    if not tree or not all(p['btns'] for m in tree for p in m['pages']):
        raise SystemExit('perm.js 解析失败：PERM_TREE 为空或存在没有按钮的页面')
    return tree


def build_permcodes(tree):
    pages = sum(len(m['pages']) for m in tree)
    btns = [b for m in tree for p in m['pages'] for b in p['btns']]
    total = len(btns)
    locked = sum(1 for b in btns if b[3])
    cnt = {k: sum(1 for b in btns if b[2] == k) for k in KIND}
    out = ['# 权限码清单', '',
           '> 本页由 `tools/build_help.py` 根据 `js/perm.js` 的 `PERM_TREE` 自动生成，请勿手工编辑。'
           '"高危"按钮点击后需要二次确认（部分还要键入确认文本）；"仅超级用户"表示锁定码，服务端只认超级用户，不能授予权限组。', '',
           f'共 **{len(tree)}** 个模块、**{pages}** 个页面、**{total}** 个权限码：'
           f'查看 {cnt["v"]} 个、操作 {cnt["o"]} 个、高危 {cnt["d"]} 个；其中 {locked} 个仅超级用户，'
           f'{total - locked} 个可授予权限组。', '']
    for m in tree:
        n = sum(len(p['btns']) for p in m['pages'])
        out += [f'## {m["name"]}（{n}）', '']
        for p in m['pages']:
            out += [f'### {p["name"]}', '', '| 权限码 | 名称 | 类型 | 仅超级用户 |', '|---|---|---|---|']
            for code, label, kind, lock in p['btns']:
                out.append(f'| `{code}` | {label} | {KIND[kind]} | {"是" if lock else ""} |')
            out.append('')
    (HELP / 'permcodes.md').write_text('\n'.join(out), encoding='utf-8')
    return total, pages


def check(md, perm_src):
    """校验 NAV_TREE 的 help 章节与站内链接，有问题直接失败。"""
    errs = []
    for h in sorted(set(re.findall(r"help: '(\w+)'", perm_src))):
        if h not in md:
            errs.append(f'perm.js 的 NAV_TREE 引用了不存在的章节 help: {h}')
    for k, text in md.items():
        for t in re.findall(r'\]\(#help/(\w+)\)', text):
            if t not in md:
                errs.append(f'{k}.md 的站内链接指向不存在的章节：#help/{t}')
        if not re.match(r'#\s+\S', text):
            errs.append(f'{k}.md 第一行必须是 "# 标题"')
    if errs:
        sys.exit('\n'.join(errs))


def main():
    if hasattr(sys.stdout, 'reconfigure'):
        sys.stdout.reconfigure(encoding='utf-8')
    perm_src = (ROOT / 'js' / 'perm.js').read_text(encoding='utf-8')
    n, pages = build_permcodes(parse_perm_tree(perm_src))
    md = {}
    for k in ORDER:
        f = HELP / f'{k}.md'
        if not f.exists():
            sys.exit(f'缺少章节文件：help/{k}.md')
        md[k] = f.read_text(encoding='utf-8')
    extra = sorted(p.stem for p in HELP.glob('*.md') if p.stem not in ORDER)
    if extra:
        print('警告：以下 help/*.md 不在 ORDER 中，未打包：' + '、'.join(extra))
    check(md, perm_src)

    js = ('/* 自动生成：python tools/build_help.py（来源 help/*.md），请勿手工编辑 */\n'
          'window.HELP_MD = ' + json.dumps(md, ensure_ascii=False, indent=0) + ';\n')
    (ROOT / 'js' / 'help_content.js').write_text(js, encoding='utf-8')

    titles = {k: re.match(r'#\s+(.*)', md[k]).group(1).strip() for k in ORDER}
    no = {k: i + 1 for i, k in enumerate(ORDER)}

    def plain_links(text):   # 合并文档里没有站内路由：把 [文字](#help/xx) 改成 "文字（见第 N 章）"
        return re.sub(r'\[([^\]]+)\]\(#help/(\w+)\)', lambda m: f'{m.group(1)}（见第 {no[m.group(2)]} 章）', text)

    doc = ['# DB Tool · 用户使用手册', '',
           '> 由 `help/*.md` 合并生成（`python tools/build_help.py`）。平台内可通过顶栏「? 帮助」或侧栏「使用手册」阅读同一内容；'
           '权限码清单一章由 `js/perm.js` 自动生成。', '',
           '## 目录', ''] + [f'{no[k]}. {titles[k]}' for k in ORDER] + ['']
    for k in ORDER:
        body = re.sub(r'^# .*\n', '', md[k], count=1)
        body = re.sub(r'^(#{2,3}) ', lambda m: '#' + m.group(1) + ' ', body, flags=re.M)   # 章节内标题下沉一级
        doc += ['', '---', '', f'## {no[k]}. {titles[k]}', plain_links(body).strip(), '']
    (ROOT / 'docs').mkdir(exist_ok=True)
    (ROOT / 'docs' / '用户使用手册.md').write_text('\n'.join(doc), encoding='utf-8')
    chars = sum(len(v) for v in md.values())
    print(f'ok: {len(ORDER)} 章，{n} 个权限码（{pages} 个页面），共 {chars} 字符 -> js/help_content.js, docs/用户使用手册.md')


if __name__ == '__main__':
    main()
