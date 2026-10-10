import { useState, useEffect, useCallback, type ReactNode } from 'react';
import { getModelStatus } from '../services/api';
import ModelFormModal from './ModelFormModal';
import { useT } from '../i18n';

/**
 * 首次配置引导：没有任何可用模型时全屏阻断，必须先配一个才能用。
 *
 * 为什么要这道门：模型是这个平台唯一的「发动机」，没配模型时新建对话、
 * 发消息都能成功入队，但任务会一直跑到解析 LLM 客户端那一步才失败，
 * 报的还是内部口径（「解析模型失败」），用户不知道发生了什么、更不知道
 * 下一步干什么。与其让每个入口各自报错，不如在应用入口一次性拦住。
 *
 * 判定口径与后端 ensureAvailableModel 一致（enabled = true 即算可用），
 * 避免「引导说没配、执行说有」这类前后端打架。
 */
export default function ModelSetupGate({ children }: { children: ReactNode }) {
  const t = useT();
  // null = 查询结果还没到（首次加载中）。
  const [blocked, setBlocked] = useState<boolean | null>(null);
  const [formOpen, setFormOpen] = useState(false);

  const check = useCallback(async () => {
    try {
      const res = await getModelStatus();
      setBlocked(!res.data.data?.has_available);
    } catch (e) {
      // 查询失败按「放行」处理。
      //
      // 为什么 fail-open：状态接口只是引导的开关，不是安全边界。后端在
      // 创建对话 / 发消息时还有一道硬校验兜底，放行不会真的放过去；
      // 反过来，若这里 fail-close，后端一抖动（或库没建表）就会把整个
      // 应用锁死在一张引导卡上，用户连「模型配置」页都进不去，无从自救。
      console.error('[model-setup-gate] 检查可用模型失败，跳过引导:', e);
      setBlocked(false);
    }
  }, []);

  useEffect(() => { check(); }, [check]);

  // 结果未到时不渲染内容，否则首页会先闪出来再被盖住。
  if (blocked === null) {
    return (
      <div className="setup-gate">
        <div className="setup-gate-card">
          <div className="setup-gate-icon">🤖</div>
          <div className="setup-gate-desc">{t('common.loading')}</div>
        </div>
      </div>
    );
  }

  if (!blocked) return <>{children}</>;

  return (
    <>
      {/* 内容照常渲染在下面：阻断靠覆盖层，保留布局上下文，
          解除引导时不会有「页面突然出现」的跳动。 */}
      {children}
      <div className="setup-gate">
        <div className="setup-gate-card">
          <div className="setup-gate-icon">🤖</div>
          <h2>{t('setup.title')}</h2>
          <p className="setup-gate-desc">{t('setup.desc')}</p>
          <ol className="setup-gate-steps">
            <li>{t('setup.step1')}</li>
            <li>{t('setup.step2')}</li>
            <li>{t('setup.step3')}</li>
          </ol>
          <button className="btn btn-primary" onClick={() => setFormOpen(true)}>
            {t('setup.addModel')}
          </button>
          <div className="setup-gate-hint">{t('setup.hint')}</div>
        </div>
      </div>
      {formOpen && (
        <ModelFormModal
          model={null}
          onClose={() => setFormOpen(false)}
          onSaved={() => {
            setFormOpen(false);
            // 重新查状态：新增的模型默认已启用，查到即可自动解除引导
            check();
          }}
        />
      )}
    </>
  );
}
