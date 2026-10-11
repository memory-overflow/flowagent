import { useState, useEffect, useCallback } from 'react';
import { useNavigate, useLocation } from 'react-router-dom';
import { listConversations, createConversation, deleteConversation, updateConversationTitle } from '../services/api';
import type { ConversationInfo } from '../types/api';
import { useI18n } from '../i18n';
import { displayTitle, isPlaceholderTitle } from '../utils/conversationTitle';

type NavKey = 'chats' | 'mcp' | 'skills' | 'models' | 'search' | 'settings';

// label 不写死中文 —— 导航项文案走 i18n（key 与 nav.<NavKey> 同名），
// 图标与路由与语言无关，留在表里。
const NAV_ITEMS: Array<{ key: NavKey; icon: string; path: string }> = [
  { key: 'chats',  icon: '💬', path: '/' },
  { key: 'mcp',    icon: '🔌', path: '/mcp' },
  { key: 'skills', icon: '🧩', path: '/skills' },
  { key: 'models', icon: '🤖', path: '/models' },
  { key: 'search', icon: '🔍', path: '/search-config' },
  { key: 'settings', icon: '⚙️', path: '/settings' },
];

export default function Sidebar() {
  const navigate = useNavigate();
  const location = useLocation();
  const { t, lang } = useI18n();
  const [convList, setConvList] = useState<ConversationInfo[]>([]);
  const [loading, setLoading] = useState(true);
  // 正在编辑标题的对话：editingId 为 chat_id，editingTitle 为编辑中的值
  const [editingId, setEditingId] = useState<string | null>(null);
  const [editingTitle, setEditingTitle] = useState('');
  // 侧边栏是否折叠（仅显示 64px 图标条，隐藏对话列表/各页面辅助区）
  const [sidebarCollapsed, setSidebarCollapsed] = useState<boolean>(() => {
    try { return localStorage.getItem('flowagent.sidebar-collapsed') === 'true'; } catch { return false; }
  });
  // 逃生重启进行中：按钮转为 ⏳ 并防重复点击
  const [restarting, setRestarting] = useState(false);

  const toggleCollapsed = useCallback(() => {
    setSidebarCollapsed((prev) => {
      const next = !prev;
      try { localStorage.setItem('flowagent.sidebar-collapsed', String(next)); } catch {}
      return next;
    });
  }, []);

  // 当前 nav 由路由推断，避免 state 同步问题
  const currentNav: NavKey = location.pathname.startsWith('/mcp')
    ? 'mcp'
    : location.pathname.startsWith('/skills')
      ? 'skills'
      : location.pathname.startsWith('/models')
        ? 'models'
        : location.pathname.startsWith('/search-config')
          ? 'search'
          : location.pathname.startsWith('/settings')
            ? 'settings'
            : 'chats';
  const activeChatId = location.pathname.startsWith('/chat/')
    ? location.pathname.split('/chat/')[1]
    : '';

  const fetchList = useCallback(async () => {
    try {
      const res = await listConversations();
      setConvList(res.data.data.list || []);
    } catch (e) {
      console.error('Failed to load conversations', e);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { fetchList(); }, [fetchList]);

  // 首次发消息时对话标题会从「新对话」变为输入的前若干字符，
  // 监听 ChatPage 派发的事件即时刷新列表，避免标题停留在「新对话」。
  useEffect(() => {
    const handler = () => fetchList();
    window.addEventListener('conversation-list-updated', handler);
    return () => window.removeEventListener('conversation-list-updated', handler);
  }, [fetchList]);

  // 有对话在执行中时轮询刷新列表。
  //
  // 为什么需要：任务可能在「别的对话页」后台完成 —— ChatPage 的
  // conversation-list-updated 事件只覆盖当前打开对话的任务收口，后台完成
  // 的任务不经过当前页的任何链路，Sidebar 会永远停在任务启动那一刻的
  // has_running_task=true 快照上（列表项一直显示「执行中」，刷新页面才恢复）。
  // 轮询补上这个盲区；列表里没有任何执行中对话时停止轮询，零空闲成本。
  const hasRunningConv = convList.some((c) => c.has_running_task);
  useEffect(() => {
    if (!hasRunningConv) return;
    const interval = setInterval(() => { fetchList(); }, 10_000);
    return () => clearInterval(interval);
  }, [hasRunningConv, fetchList]);

  // 切到 /mcp 路由时，停止拉对话列表以节省请求
  useEffect(() => {
    if (currentNav === 'chats' && convList.length === 0 && !loading) {
      fetchList();
    }
  }, [currentNav]);

  // 逃生入口：直连 supervisor（127.0.0.1:9090）强制重启后端，
  // 不经过可能已卡死的 server(:8080)。触发后轮询 supervisor 健康，
  // 直到两个子进程都被拉起，再刷新页面回到可用状态。
  const handleRestart = async () => {
    if (restarting) return;
    if (!confirm(t('nav.restartConfirm'))) return;
    setRestarting(true);
    try {
      const res = await fetch('http://127.0.0.1:9090/restart', { method: 'POST' });
      if (!res.ok && res.status !== 202) throw new Error(`HTTP ${res.status}`);
      const deadline = Date.now() + 30_000;
      let up = false;
      while (Date.now() < deadline) {
        await new Promise((r) => setTimeout(r, 1500));
        try {
          const h = await fetch('http://127.0.0.1:9090/health');
          if ((await h.json())?.data?.all_up) { up = true; break; }
        } catch { /* 重启窗口期 supervisor/子进程短暂不可达属正常，继续等 */ }
      }
      if (up) {
        alert(t('nav.restartDone'));
        window.location.reload();
      } else {
        alert(t('nav.restartTimeout'));
      }
    } catch (e: any) {
      alert(t('nav.restartFailed', { msg: e.message }));
    } finally {
      setRestarting(false);
    }
  };

  const handleNew = async () => {
    try {
      // 不传标题：占位标题由后端按界面语言落库，避免前端文案与后端占位判定对不上
      const res = await createConversation();
      const chatId = res.data.data.chat_id;
      await fetchList();
      navigate(`/chat/${chatId}`);
    } catch (e: any) {
      alert(t('sidebar.createFailed', { msg: e.message }));
    }
  };

  const handleDelete = async (e: React.MouseEvent, chatId: string) => {
    e.stopPropagation();
    if (!confirm(t('nav.deleteConfirm'))) return;
    try {
      await deleteConversation(chatId);
      setConvList((prev) => prev.filter((c) => c.chat_id !== chatId));
      if (activeChatId === chatId) navigate('/');
    } catch (err: any) {
      alert(t('sidebar.deleteFailed', { msg: err.message }));
    }
  };

  const startEdit = (e: React.MouseEvent, chatId: string, title: string) => {
    e.stopPropagation();
    setEditingId(chatId);
    // 占位标题不进编辑框，用户直接敲新标题即可，不用先删掉「新对话」
    setEditingTitle(isPlaceholderTitle(title) ? '' : title || '');
  };

  const commitEdit = async () => {
    const chatId = editingId;
    if (!chatId) return;
    const title = editingTitle.trim();
    setEditingId(null);
    if (!title) return; // 空标题视为放弃本次编辑，保持原标题
    try {
      await updateConversationTitle(chatId, title);
      setConvList((prev) =>
        prev.map((c) => (c.chat_id === chatId ? { ...c, title } : c)),
      );
      // 通知 ChatPage 头部标题同步刷新
      window.dispatchEvent(new Event('conversation-list-updated'));
    } catch (err: any) {
      alert(t('sidebar.renameFailed', { msg: err.message }));
    }
  };

  const cancelEdit = () => {
    setEditingId(null);
    setEditingTitle('');
  };

  // 时间格式跟随界面语言：中文给「14:05 / 9月28日」，英文给「2:05 PM / Sep 28」。
  // 写死 zh-CN 会让英文界面下出现中文月份。
  const timeLocale = lang === 'zh' ? 'zh-CN' : 'en-US';
  const formatTime = (ts: number) => {
    if (!ts) return '';
    const d = new Date(ts);
    const now = new Date();
    const isToday = d.toDateString() === now.toDateString();
    if (isToday) {
      return d.toLocaleTimeString(timeLocale, { hour: '2-digit', minute: '2-digit' });
    }
    return d.toLocaleDateString(timeLocale, { month: 'short', day: 'numeric' });
  };

  return (
    <aside className={`sidebar ${sidebarCollapsed ? 'collapsed' : ''}`}>
      {/* 左侧竖直导航条 */}
      <nav className="nav-rail">
        <div className="nav-rail-logo" title={t('welcome.title')}>⚡</div>
        {NAV_ITEMS.map((item) => (
          <button
            key={item.key}
            className={`nav-rail-item ${currentNav === item.key ? 'active' : ''}`}
            onClick={() => navigate(item.path)}
            title={t(`nav.${item.key}`)}
          >
            <span className="nav-rail-icon">{item.icon}</span>
            <span className="nav-rail-label">{t(`nav.${item.key}`)}</span>
          </button>
        ))}
        {/* 底部：逃生重启按钮（常驻）+ 折叠态的展开按钮 */}
        <div className="nav-rail-spacer" />
        {sidebarCollapsed && (
          <button
            className="nav-rail-toggle"
            onClick={toggleCollapsed}
            title={t('nav.expandSidebar')}
          >
            ›
          </button>
        )}
        <button
          className="nav-rail-item"
          onClick={handleRestart}
          disabled={restarting}
          title={t('nav.restartHint')}
        >
          <span className="nav-rail-icon">{restarting ? '⏳' : '🔄'}</span>
          <span className="nav-rail-label">{t('nav.restartBackend')}</span>
        </button>
      </nav>

      {/* 右侧内容区：对话列表 / MCP 页面内的辅助列表 */}
      <div className="sidebar-content">
        {currentNav === 'chats' && (
          <>
            <div className="sidebar-header">
              <span className="logo">{t('nav.chats')}</span>
              <div className="header-actions">
                <button className="btn btn-primary btn-sm" onClick={handleNew}>
                  {t('sidebar.newShort')}
                </button>
                <button
                  className="btn-collapse"
                  onClick={toggleCollapsed}
                  title={t('nav.collapseSidebar')}
                >
                  ‹
                </button>
              </div>
            </div>

            <div className="conv-list">
              {loading && <div style={{ padding: 20, textAlign: 'center', color: 'var(--text-muted)' }}>{t('common.loading')}</div>}
              {!loading && convList.length === 0 && (
                <div className="empty-state" style={{ padding: '30px 0' }}>
                  <div className="empty-desc">{t('sidebar.emptyHint')}</div>
                </div>
              )}
              {convList.map((c) => (
                <div
                  key={c.chat_id}
                  className={`conv-item ${activeChatId === c.chat_id ? 'active' : ''}`}
                  onClick={() => navigate(`/chat/${c.chat_id}`)}
                >
                  <div className="conv-icon">💬</div>
                  <div className="conv-info">
                    {editingId === c.chat_id ? (
                      <input
                        className="conv-title-input"
                        autoFocus
                        value={editingTitle}
                        onChange={(e) => setEditingTitle(e.target.value)}
                        onBlur={commitEdit}
                        onKeyDown={(e) => {
                          if (e.key === 'Enter') commitEdit();
                          else if (e.key === 'Escape') cancelEdit();
                        }}
                        onClick={(e) => e.stopPropagation()}
                      />
                    ) : (
                      <div className="conv-title" title={displayTitle(c.title, t('nav.newChat'), t('sidebar.untitled'))}>
                        {displayTitle(c.title, t('nav.newChat'), t('sidebar.untitled'))}
                      </div>
                    )}
                    <div className="conv-meta">
                      <span className="conv-time">{formatTime(c.update_ts)}</span>
                      {c.has_running_task && (
                        <span className="status-dot running" title={t('sidebar.runningTitle')}>{t('status.running')}</span>
                      )}
                      {!c.has_running_task && c.has_failed_task && (
                        <span className="status-dot failed" title={t('sidebar.failedTitle')}>{t('status.failed')}</span>
                      )}
                    </div>
                  </div>
                  <div className="conv-actions">
                    <button
                      className="btn btn-ghost btn-icon btn-sm"
                      onClick={(e) => startEdit(e, c.chat_id, c.title)}
                      title={t('sidebar.rename')}
                    >
                      ✎
                    </button>
                    <button
                      className="btn btn-ghost btn-icon btn-sm"
                      onClick={(e) => handleDelete(e, c.chat_id)}
                      title={t('common.delete')}
                    >
                      ×
                    </button>
                  </div>
                </div>
              ))}
            </div>
          </>
        )}

        {currentNav === 'mcp' && (
          <>
            <div className="sidebar-header">
              <span className="logo">{t('sidebar.mcpTitle')}</span>
              <div className="header-actions">
                <button
                  className="btn-collapse"
                  onClick={toggleCollapsed}
                  title={t('nav.collapseSidebar')}
                >
                  ‹
                </button>
              </div>
            </div>
            <div className="sidebar-hint">
              <div className="hint-icon">🔌</div>
              <div className="hint-title">{t('sidebar.mcpHintTitle')}</div>
              <div className="hint-desc">
                {t('sidebar.mcpHintDesc')}
              </div>
            </div>
          </>
        )}

        {currentNav === 'skills' && (
          <>
            <div className="sidebar-header">
              <span className="logo">{t('sidebar.skillsTitle')}</span>
              <div className="header-actions">
                <button
                  className="btn-collapse"
                  onClick={toggleCollapsed}
                  title={t('nav.collapseSidebar')}
                >
                  ‹
                </button>
              </div>
            </div>
            <div className="sidebar-hint">
              <div className="hint-icon">🧩</div>
              <div className="hint-title">{t('sidebar.skillsHintTitle')}</div>
              <div className="hint-desc">
                {t('sidebar.skillsHintDesc')}
              </div>
            </div>
          </>
        )}

        {currentNav === 'models' && (
          <>
            <div className="sidebar-header">
              <span className="logo">{t('sidebar.modelsTitle')}</span>
              <div className="header-actions">
                <button
                  className="btn-collapse"
                  onClick={toggleCollapsed}
                  title={t('nav.collapseSidebar')}
                >
                  ‹
                </button>
              </div>
            </div>
            <div className="sidebar-hint">
              <div className="hint-icon">🤖</div>
              <div className="hint-title">{t('sidebar.modelsHintTitle')}</div>
              <div className="hint-desc">
                {t('sidebar.modelsHintDesc')}
              </div>
            </div>
          </>
        )}

        {currentNav === 'search' && (
          <>
            <div className="sidebar-header">
              <span className="logo">{t('sidebar.searchTitle')}</span>
              <div className="header-actions">
                <button
                  className="btn-collapse"
                  onClick={toggleCollapsed}
                  title={t('nav.collapseSidebar')}
                >
                  ‹
                </button>
              </div>
            </div>
            <div className="sidebar-hint">
              <div className="hint-icon">🔍</div>
              <div className="hint-title">{t('sidebar.searchHintTitle')}</div>
              <div className="hint-desc">
                {t('sidebar.searchHintDesc')}
              </div>
            </div>
          </>
        )}
      </div>
    </aside>
  );
}
