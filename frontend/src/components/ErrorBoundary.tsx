import { Component, ReactNode } from 'react';
import { useT } from '../i18n';

interface Props {
  children: ReactNode;
}

interface State {
  error: Error | null;
}

// 错误态 UI 单独抽成函数组件：ErrorBoundary 本身必须是 class（要用
// getDerivedStateFromError），而 class 里用不了 hook，拿不到 t。
function ErrorView({ error, onReset }: { error: Error; onReset: () => void }) {
  const t = useT();
  return (
    <div style={{ padding: 24, maxWidth: 720, margin: '40px auto' }}>
      <div style={{ fontSize: 20, fontWeight: 600, marginBottom: 12, color: 'var(--danger)' }}>
        {t('errorBoundary.title')}
      </div>
      <div style={{ fontSize: 13, color: 'var(--text-muted)', marginBottom: 12 }}>
        {t('errorBoundary.desc')}
      </div>
      <pre
        style={{
          // 原先写死 #f5f5f5：深色主题下正文是浅色，压在浅灰底上几乎看不见。
          background: 'var(--bg-tertiary)',
          padding: 12,
          borderRadius: 6,
          fontSize: 12,
          overflow: 'auto',
          maxHeight: 320,
          whiteSpace: 'pre-wrap',
          wordBreak: 'break-word',
        }}
      >
        {error.stack || error.message}
      </pre>
      <div style={{ marginTop: 12, display: 'flex', gap: 8 }}>
        <button className="btn btn-primary btn-sm" onClick={() => window.location.reload()}>
          {t('errorBoundary.reload')}
        </button>
        <button className="btn btn-ghost btn-sm" onClick={onReset}>
          {t('errorBoundary.recover')}
        </button>
      </div>
    </div>
  );
}

export default class ErrorBoundary extends Component<Props, State> {
  state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  componentDidCatch(error: Error, info: React.ErrorInfo) {
    // eslint-disable-next-line no-console
    console.error('[ErrorBoundary]', error, info);
  }

  handleReset = () => {
    this.setState({ error: null });
  };

  render() {
    if (this.state.error) {
      return <ErrorView error={this.state.error} onReset={this.handleReset} />;
    }
    return this.props.children;
  }
}
