import { Fragment, useEffect, useRef } from 'react';

/**
 * 引用候选项。@ 与 # 共用一套结构 —— 两者只差候选来源与插入字面量，
 * 渲染与键盘交互完全一致，拆成两个组件只会得到两份一样的列表代码。
 */
export interface MentionItem {
  /** 唯一键，用于 React 列表 */
  id: string;
  /** 分组标题。相邻同组项只渲染一次标题；筛选后同组可能被拆开，标题会重复出现，可接受 */
  group?: string;
  icon?: string;
  /** 主文案 */
  label: string;
  /** 副文案（路径 / 描述） */
  desc?: string;
  /** 右侧标记，如「我上传的」「产物」「已启用」 */
  tag?: string;
  /** 选中后插入输入框的字面量（不含触发符） */
  insert: string;
  /** 选中后的额外动作（如把技能挂到本对话、执行本地指令） */
  onPick?: () => void;
}

/**
 * 输入框引用菜单：浮在输入卡片上方，列出当前触发符的可选项。
 *
 * 键盘交互（上下移动 / 回车选中 / Esc 关闭）由输入框的 keydown 统一处理，
 * 本组件只负责渲染与鼠标事件 —— 焦点始终留在 textarea 上，
 * 否则用户打字打到一半会被焦点跳走打断。
 */
export default function InputMentionMenu({
  title,
  emptyText,
  footer,
  items,
  activeIndex,
  onHover,
  onPick,
}: {
  title: string;
  emptyText: string;
  footer: string;
  items: MentionItem[];
  activeIndex: number;
  onHover: (index: number) => void;
  onPick: (item: MentionItem) => void;
}) {
  const listRef = useRef<HTMLDivElement>(null);

  // 键盘移动高亮时把选中项滚进可视区。候选比容器长时（技能多的对话很常见），
  // 不滚的话用户按方向键看不到自己选到了哪一项。
  useEffect(() => {
    const el = listRef.current?.querySelector<HTMLElement>(`[data-idx="${activeIndex}"]`);
    el?.scrollIntoView({ block: 'nearest' });
  }, [activeIndex, items.length]);

  return (
    <div className="mention-menu" role="listbox" aria-label={title}>
      <div className="mention-menu-head">{title}</div>
      {items.length === 0 ? (
        <div className="mention-menu-empty">{emptyText}</div>
      ) : (
        <div className="mention-menu-list" ref={listRef}>
          {items.map((item, idx) => (
            <Fragment key={item.id}>
              {item.group && item.group !== items[idx - 1]?.group && (
                <div className="mention-menu-group">{item.group}</div>
              )}
              <div
                data-idx={idx}
                className={`mention-menu-item${idx === activeIndex ? ' is-active' : ''}`}
                role="option"
                aria-selected={idx === activeIndex}
                onMouseEnter={() => onHover(idx)}
                // 用 mousedown 而不是 click：click 之前 textarea 会先失焦，
                // 插入后光标位置就没了。preventDefault 保住焦点。
                onMouseDown={(e) => {
                  e.preventDefault();
                  onPick(item);
                }}
              >
                {item.icon && (
                  <span className="mention-menu-icon" aria-hidden>
                    {item.icon}
                  </span>
                )}
                <span className="mention-menu-label">{item.label}</span>
                {item.desc && <span className="mention-menu-desc">{item.desc}</span>}
                {item.tag && <span className="mention-menu-tag">{item.tag}</span>}
              </div>
            </Fragment>
          ))}
        </div>
      )}
      <div className="mention-menu-foot">{footer}</div>
    </div>
  );
}
