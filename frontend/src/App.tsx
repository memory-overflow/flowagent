import { lazy, Suspense } from 'react';
import { Routes, Route, Navigate, useNavigate } from 'react-router-dom';
import Sidebar from './components/Sidebar';
import ChatPage from './pages/ChatPage';
import MCPServersPage from './pages/MCPServersPage';
import SkillsPage from './pages/SkillsPage';
import ModelsPage from './pages/ModelsPage';
import SearchConfigPage from './pages/SearchConfigPage';
import SettingsPage from './pages/SettingsPage';
import DebugPage from './pages/DebugPage';
import ErrorBoundary from './components/ErrorBoundary';
import ModelSetupGate from './components/ModelSetupGate';
import { createConversation } from './services/api';
import { useT } from './i18n';
import './App.css';

// 工作流页面懒加载：它依赖 @xyflow/react（体积较大），
// 只有进入工作流页才加载，对话页不为此买单。
const WorkflowPage = lazy(() => import('./pages/WorkflowPage'));

export default function App() {
  const t = useT();
  return (
    // 没有任何可用模型时全屏阻断，引导用户先配一个（判定与后端一致）。
    // 包在最外层：无论用户从哪个路由进来都逃不掉，也不用每个页面各判一次。
    <ModelSetupGate>
      <div className="app-layout">
        <Sidebar />
        <main className="main-area">
          <ErrorBoundary>
            <Routes>
              <Route path="/" element={<WelcomeView />} />
              <Route path="/chat/:chatId" element={<ChatPage />} />
              <Route
                path="/chat/:chatId/workflow"
                element={
                  <Suspense fallback={<div className="empty-state"><div className="empty-desc">{t('common.loading')}</div></div>}>
                    <WorkflowPage />
                  </Suspense>
                }
              />
              <Route path="/debug/:taskId" element={<DebugPage />} />
              <Route path="/mcp" element={<MCPServersPage />} />
              <Route path="/skills" element={<SkillsPage />} />
              <Route path="/models" element={<ModelsPage />} />
              <Route path="/search-config" element={<SearchConfigPage />} />
              <Route path="/settings" element={<SettingsPage />} />
              <Route path="*" element={<Navigate to="/" replace />} />
            </Routes>
          </ErrorBoundary>
        </main>
      </div>
    </ModelSetupGate>
  );
}

function WelcomeView() {
  const navigate = useNavigate();
  const t = useT();
  return (
    <div className="welcome-view">
      <div className="welcome-icon">⚡</div>
      <h1>{t('welcome.title')}</h1>
      <p>{t('welcome.desc')}</p>
      <div style={{ display: 'flex', gap: 10 }}>
        <button className="btn btn-primary" onClick={async () => {
          try {
            // 不传标题：占位标题由后端按界面语言落库，避免前端文案与后端占位判定对不上
            const res = await createConversation();
            const chatId = res.data.data.chat_id;
            navigate(`/chat/${chatId}`);
          } catch (e: any) {
            alert(t('welcome.createFailed', { msg: e.message }));
          }
        }}>
          {t('welcome.newChat')}
        </button>
      </div>
    </div>
  );
}
