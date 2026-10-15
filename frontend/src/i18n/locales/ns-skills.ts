// skills 页面/组件的文案。zh 与 en 成对维护（见同目录 index.ts 的说明）。
export const skillsNS = {
  zh: {
    title: 'Skill 技能管理',
    importBuiltin: '一键导入内置 Skill',
    importSkill: '+ 导入技能',
    emptyTitle: '暂无 Skill',
    emptyDesc:
      'Skill 是可复用的能力包：一段注入 system prompt 的指令，可附带随包脚本（由模型通过 run_command 执行）。点击上方按钮创建或导入。',

    updatedAt: '更新时间',

    deleteConfirm: '确定删除此 Skill？',
    deleteFailed: '删除失败: {msg}',

    done: '完成',

    import: {
      title: '导入技能',
      dropzone: '拖拽文件或点击上传',
      importing: '导入中…',
      needZipOrMd: '请选择 .zip 或 .md 文件',
      failed: '导入失败: {msg}',
      overwrite: '同名 Skill 自动覆盖更新',
      reqTitle: '文件要求',
      req1Lead: '.zip 包需包含 ',
      req1Mid: ' 或 ',
      req1Tail: ' 清单（脚本随包落地到 skills/ 目录）',
      req2Tail: ' 需包含 YAML 格式的技能名称和描述；集合包（根 + 多个子 skill）会合并为一条父 Skill',
      req3: '想手动填写配置？点击下方「手动新建 Skill」',
      complete: '导入完成：共 {total} 条（新建 {created} / 覆盖 {updated} / 跳过 {skipped}），落地文件 {files} 个',
      tagCreated: '（新建）',
      tagUpdated: '（覆盖）',
      manualCreate: '手动新建 Skill →',
    },

    form: {
      createTitle: '新建 Skill',
      editTitle: '编辑 Skill',
      name: '名称 *',
      namePlaceholder: '例如: web_summary, text_stats',
      content: '指令正文（markdown）',
      contentPlaceholder: '写一段给模型的指令，例如：你是写作助手，输出前先列出大纲……；需要运行脚本时写明脚本路径与命令用法',
      descPlaceholder: '简要描述该 Skill 的功能',
      nameRequired: '请输入名称',
      saveFailed: '保存失败: {msg}',
    },

    builtin: {
      source: '清单来源：',
      emptyTitle: '目录下暂无内置清单',
      emptyDescLead: '在项目 ',
      emptyDescMid: ' 目录下放置 ',
      emptyDescTail: ' 清单即可显示在这里。',
      exists: '已存在',
      overwrite: '已存在同名记录时覆盖（更新配置）',
      selectAtLeastOne: '请至少选中一个内置 Skill',
      importFailed: '导入失败: {msg}',
      importing: '导入中...',
      import: '导入选中 ({n})',
      resultCreated: '新建：',
      resultUpdated: '更新：',
      resultSkipped: '跳过：',
      resultNone: '（无）',
      listSep: '、',
    },
  },

  en: {
    title: 'Skills',
    importBuiltin: 'Import built-in skills',
    importSkill: '+ Import skill',
    emptyTitle: 'No skills yet',
    emptyDesc:
      'Skills are reusable capability packs: instructions injected into the system prompt, optionally bundled with scripts that the model runs via run_command. Use the buttons above to create or import one.',

    updatedAt: 'Updated',

    deleteConfirm: 'Delete this skill?',
    deleteFailed: 'Failed to delete: {msg}',

    done: 'Done',

    import: {
      title: 'Import skill',
      dropzone: 'Drag a file here or click to upload',
      importing: 'Importing…',
      needZipOrMd: 'Please select a .zip or .md file',
      failed: 'Failed to import: {msg}',
      overwrite: 'Overwrite skills with the same name',
      reqTitle: 'File requirements',
      req1Lead: 'The .zip must contain a ',
      req1Mid: ' or ',
      req1Tail: ' manifest (scripts are unpacked into the skills/ directory)',
      req2Tail:
        ' must declare the skill name and description as YAML frontmatter; bundle packs (a root plus several child skills) are merged into one parent skill',
      req3: 'Prefer to fill in the config by hand? Use “Create skill manually” below.',
      complete:
        'Import complete: {total} skills (created {created} / updated {updated} / skipped {skipped}), {files} files written',
      tagCreated: '(created)',
      tagUpdated: '(updated)',
      manualCreate: 'Create skill manually →',
    },

    form: {
      createTitle: 'New skill',
      editTitle: 'Edit skill',
      name: 'Name *',
      namePlaceholder: 'e.g. web_summary, text_stats',
      content: 'Instructions (markdown)',
      contentPlaceholder:
        'Write instructions for the model, e.g. “You are a writing assistant — outline first, then draft…”; if scripts are involved, document their paths and usage',
      descPlaceholder: 'Briefly describe what this skill does',
      nameRequired: 'Please enter a name',
      saveFailed: 'Failed to save: {msg}',
    },

    builtin: {
      source: 'Manifest source: ',
      emptyTitle: 'No built-in manifests in this directory',
      emptyDescLead: "In the project's ",
      emptyDescMid: ' directory, drop ',
      emptyDescTail: ' manifests to list them here.',
      exists: 'Already added',
      overwrite: 'Overwrite entries with the same name (update their config)',
      selectAtLeastOne: 'Select at least one built-in skill',
      importFailed: 'Failed to import: {msg}',
      importing: 'Importing...',
      import: 'Import selected ({n})',
      resultCreated: 'Created: ',
      resultUpdated: 'Updated: ',
      resultSkipped: 'Skipped: ',
      resultNone: '(none)',
      listSep: ', ',
    },
  },
};
