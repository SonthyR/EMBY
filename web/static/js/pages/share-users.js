// Share users: Meridian-issued Emby logins that friends use on proxied sites.
const SHARE_GB = 1024 * 1024 * 1024;
let shareUsersCache = [];
let shareSitesCache = [];

function shareFormatBytes(value) {
  const bytes = Number(value) || 0;
  if (bytes <= 0) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let size = bytes;
  let unit = 0;
  while (size >= 1024 && unit < units.length - 1) { size /= 1024; unit++; }
  return unit === 0 ? `${bytes} B` : `${size.toFixed(2)} ${units[unit]}`;
}

function shareStatusPill(status) {
  const map = {
    active: ['pill-green', '正常'],
    disabled: ['', '已停用'],
    expired: ['pill-orange', '已过期'],
    quota: ['pill-orange', '流量用完'],
  };
  const [cls, label] = map[status] || ['', status || '未知'];
  return `<span class="pill ${cls}">${esc(label)}</span>`;
}

function shareExpiryLabel(value) {
  const raw = String(value || '').trim();
  if (!raw) return '永久';
  return raw.slice(0, 10);
}

async function shareCopyText(text, successMessage) {
  try {
    if (navigator.clipboard && typeof navigator.clipboard.writeText === 'function') {
      await navigator.clipboard.writeText(text);
    } else {
      const input = document.createElement('textarea');
      input.value = text;
      input.setAttribute('readonly', '');
      input.style.position = 'fixed';
      input.style.opacity = '0';
      document.body.appendChild(input);
      input.select();
      const copied = document.execCommand('copy');
      input.remove();
      if (!copied) throw new Error('copy failed');
    }
    Toast.success(successMessage);
  } catch (_) {
    shareShowText(text);
  }
}

function shareShowText(text) {
  document.getElementById('modal-title').textContent = '分享信息';
  document.getElementById('modal-body').innerHTML = `<p class="form-help">复制下面的内容发给朋友：</p><textarea class="form-input share-text-area" id="share-text-area" rows="12" readonly>${esc(text)}</textarea>`;
  document.getElementById('modal-footer').innerHTML = '<button class="btn-modal secondary" id="share-text-close">关闭</button><button class="btn-modal primary" id="share-text-copy">复制</button>';
  document.getElementById('share-text-close').onclick = closeModal;
  document.getElementById('share-text-copy').onclick = () => {
    const area = document.getElementById('share-text-area');
    area.select();
    try { document.execCommand('copy'); Toast.success('已复制'); } catch (_) { Toast.error('请手动选中复制'); }
  };
  openModal({ closeOnBackdrop: true });
}

function renderShareUserRows(users) {
  if (!users.length) {
    return `<div class="empty-state">
      <h2>还没有分享账号</h2>
      <p>创建一个分享账号，朋友就能用它在 Emby 客户端登录你的站点，不需要知道源站账号密码。</p>
      <button class="btn-primary" type="button" data-share-action="create">新建分享账号</button>
    </div>`;
  }
  return `<div class="share-user-list">${users.map(user => {
    const sites = (user.sites || []).map(site => `<span class="pill pill-blue">${esc(site.site_name || ('站点 ' + site.site_id))}</span>`).join(' ');
    const quota = Number(user.traffic_quota) > 0 ? shareFormatBytes(user.traffic_quota) : '不限';
    const percent = Number(user.traffic_quota) > 0 ? Math.min(100, Math.round(Number(user.traffic_used) / Number(user.traffic_quota) * 100)) : 0;
    return `<article class="share-user-card" data-share-id="${user.id}">
      <header class="share-user-head">
        <div class="share-user-title"><strong>${esc(user.username)}</strong> ${shareStatusPill(user.status)}</div>
        <div class="share-user-actions">
          <button type="button" class="btn-modal primary" data-share-action="copy">复制分享信息</button>
          <button type="button" class="btn-modal secondary" data-share-action="portal">查询链接</button>
          <button type="button" class="btn-modal secondary" data-share-action="edit">编辑</button>
          <button type="button" class="btn-modal secondary" data-share-action="more">更多</button>
        </div>
      </header>
      <dl class="share-user-meta">
        <div><dt>密码</dt><dd class="mono">${user.password ? `<span class="share-secret" data-secret="${esc(user.password)}">••••••</span> <button type="button" class="share-link-btn" data-share-action="reveal">显示</button>` : '—'}</dd></div>
        <div><dt>到期</dt><dd>${esc(shareExpiryLabel(user.expires_at))}</dd></div>
        <div><dt>流量</dt><dd>${esc(shareFormatBytes(user.traffic_used))} / ${esc(quota)}${Number(user.traffic_quota) > 0 ? `<span class="share-usage-bar"><span style="width:${percent}%"></span></span>` : ''}</dd></div>
        <div><dt>在线设备</dt><dd>${Number(user.session_count) || 0}${Number(user.max_devices) > 0 ? ` / 最多 ${Number(user.max_devices)}` : ''}</dd></div>
        ${Number(user.speed_limit) > 0 ? `<div><dt>限速</dt><dd>${Number(user.speed_limit)} Mbps</dd></div>` : ''}
        ${user.next_reset_at ? `<div><dt>下次流量重置</dt><dd>${esc(user.next_reset_at)}</dd></div>` : ''}
      </dl>
      <div class="share-user-sites">${sites || '<span class="form-help">未分配站点</span>'}</div>
      ${user.note ? `<p class="share-user-note">${esc(user.note)}</p>` : ''}
    </article>`;
  }).join('')}</div>`;
}

async function loadShareUsers() {
  const list = document.getElementById('share-user-container');
  if (!list) return;
  try {
    const [users, sites] = await Promise.all([API.listShareUsers(), API.listSites()]);
    if (Router.current !== 'share-users') return;
    shareUsersCache = Array.isArray(users) ? users : [];
    shareSitesCache = Array.isArray(sites) ? sites : (sites && Array.isArray(sites.sites) ? sites.sites : []);
    const summary = document.getElementById('share-user-summary');
    if (summary) summary.innerHTML = `共 <strong>${shareUsersCache.length}</strong> 个分享账号`;
    list.innerHTML = renderShareUserRows(shareUsersCache);
  } catch (error) {
    if (Router.current === 'share-users') list.innerHTML = `<p class="form-help">读取失败：${esc(error.message || '')}</p>`;
  }
}

async function shareSiteAccountOptions(siteId, selected) {
  let accounts = [];
  try { accounts = await API.listSiteAccounts(siteId); } catch (_) { accounts = []; }
  const usable = (accounts || []).filter(account => account.password || account.password_configured);
  const options = [`<option value="0">自动（第一个有密码的账号）</option>`].concat(usable.map(account =>
    `<option value="${account.id}" ${Number(selected) === Number(account.id) ? 'selected' : ''}>${esc(account.account_name || account.username)}</option>`));
  return { html: options.join(''), usable: usable.length };
}

async function openShareUserForm(user) {
  const editing = !!user;
  const assigned = new Map((user && user.sites || []).map(site => [Number(site.site_id), Number(site.site_account_id) || 0]));
  const siteRows = await Promise.all(shareSitesCache.map(async site => {
    const { html, usable } = await shareSiteAccountOptions(site.id, assigned.get(Number(site.id)) || 0);
    return `<div class="share-site-row">
      <label class="share-site-check"><input type="checkbox" data-share-site="${site.id}" ${assigned.has(Number(site.id)) ? 'checked' : ''}> ${esc(site.name)}${site.enabled === false ? ' <span class="pill">已停用</span>' : ''}</label>
      <select class="form-select share-site-account" data-share-site-account="${site.id}" ${usable ? '' : 'disabled'}>${html}</select>
      ${usable ? '' : '<span class="form-help share-site-warn">该站点还没有保存带密码的源站账号</span>'}
    </div>`;
  }));
  const quotaGB = user && Number(user.traffic_quota) > 0 ? +(Number(user.traffic_quota) / SHARE_GB).toFixed(2) : '';
  document.getElementById('modal-title').textContent = editing ? '编辑分享账号' : '新建分享账号';
  document.getElementById('modal-body').innerHTML = `
    <form id="share-user-form" class="share-user-form" autocomplete="off">
      <div class="form-group"><label for="share-username">用户名</label>
        <input class="form-input" id="share-username" maxlength="64" autocapitalize="none" spellcheck="false" value="${esc(user ? user.username : '')}" required></div>
      <div class="form-group"><label for="share-password">密码</label>
        <input class="form-input" id="share-password" maxlength="72" autocapitalize="none" spellcheck="false" placeholder="${editing ? '不修改请留空' : '留空自动生成'}">
        <div class="form-help">${editing ? '修改密码后，已登录的设备会被踢下线。' : '至少 4 个字符。'}</div></div>
      <div class="share-form-grid">
        <div class="form-group"><label for="share-expires">到期日期</label>
          <input class="form-input" id="share-expires" type="date" value="${esc(user && user.expires_at ? String(user.expires_at).slice(0, 10) : '')}">
          <div class="form-help">留空表示永久。</div></div>
        <div class="form-group"><label for="share-quota">流量额度（GB）</label>
          <input class="form-input" id="share-quota" type="number" min="0" step="0.01" value="${quotaGB}" placeholder="不限">
          <div class="form-help">上行加下行合计，留空或 0 表示不限。</div></div>
      </div>
      <div class="share-form-grid">
        <div class="form-group"><label for="share-reset-day">每月流量重置日</label>
          <select class="form-select" id="share-reset-day">
            <option value="0">不自动重置</option>
            ${Array.from({ length: 28 }, (_, i) => i + 1).map(day => `<option value="${day}" ${user && Number(user.traffic_reset_day) === day ? 'selected' : ''}>每月 ${day} 日</option>`).join('')}
          </select>
          <div class="form-help">到这一天自动把已用流量清零。</div></div>
        <div class="form-group"><label for="share-max-devices">同时在线设备数</label>
          <input class="form-input" id="share-max-devices" type="number" min="0" max="100" step="1" value="${user && Number(user.max_devices) > 0 ? Number(user.max_devices) : ''}" placeholder="不限">
          <div class="form-help">超出时自动踢掉最久没用的设备，留空或 0 表示不限。</div></div>
      </div>
      <div class="share-form-grid">
        <div class="form-group"><label for="share-speed">限速（Mbps）</label>
          <input class="form-input" id="share-speed" type="number" min="0" max="100000" step="1" value="${user && Number(user.speed_limit) > 0 ? Number(user.speed_limit) : ''}" placeholder="不限">
          <div class="form-help">这个账号所有设备合计的下载速度上限，留空或 0 表示不限。1080p 约需 10–20，4K 约需 40–80。</div></div>
      </div>
      <label class="share-site-check"><input type="checkbox" id="share-enabled" ${!user || user.enabled ? 'checked' : ''}> 启用</label>
      <div class="form-group"><label>可用站点与源站账号</label>
        <div class="share-site-list">${siteRows.join('') || '<p class="form-help">还没有站点。</p>'}</div></div>
      <div class="form-group"><label for="share-note">备注</label>
        <input class="form-input" id="share-note" maxlength="1000" value="${esc(user ? user.note || '' : '')}" placeholder="例如：朋友名字"></div>
    </form>`;
  document.getElementById('modal-footer').innerHTML = `<button class="btn-modal secondary" id="share-cancel">取消</button><button class="btn-modal primary" id="share-save">${editing ? '保存' : '创建'}</button>`;
  document.getElementById('share-cancel').onclick = closeModal;
  document.getElementById('share-save').onclick = async event => {
    const button = event.currentTarget;
    const username = document.getElementById('share-username').value.trim();
    const password = document.getElementById('share-password').value;
    const quotaValue = document.getElementById('share-quota').value.trim();
    const quota = quotaValue === '' ? 0 : Number(quotaValue);
    if (!username) return Toast.error('请填写用户名');
    if (password && Array.from(password).length < 4) return Toast.error('密码至少 4 个字符');
    if (!Number.isFinite(quota) || quota < 0) return Toast.error('流量额度无效');
    const devicesValue = document.getElementById('share-max-devices').value.trim();
    const maxDevices = devicesValue === '' ? 0 : Number(devicesValue);
    if (!Number.isInteger(maxDevices) || maxDevices < 0 || maxDevices > 100) return Toast.error('同时在线设备数应为 0–100');
    const speedValue = document.getElementById('share-speed').value.trim();
    const speedLimit = speedValue === '' ? 0 : Number(speedValue);
    if (!Number.isInteger(speedLimit) || speedLimit < 0 || speedLimit > 100000) return Toast.error('限速应为 0–100000 的整数');
    const sites = [];
    document.querySelectorAll('[data-share-site]').forEach(box => {
      if (!box.checked) return;
      const siteId = Number(box.dataset.shareSite);
      const select = document.querySelector(`[data-share-site-account="${siteId}"]`);
      sites.push({ site_id: siteId, site_account_id: Number(select && select.value) || 0 });
    });
    if (!sites.length) return Toast.error('请至少选择一个站点');
    const payload = {
      username,
      password,
      note: document.getElementById('share-note').value.trim(),
      enabled: document.getElementById('share-enabled').checked,
      expires_at: document.getElementById('share-expires').value,
      traffic_quota: Math.round(quota * SHARE_GB),
      max_devices: maxDevices,
      speed_limit: speedLimit,
      traffic_reset_day: Number(document.getElementById('share-reset-day').value) || 0,
      sites,
    };
    button.disabled = true;
    try {
      const saved = editing ? await API.updateShareUser(user.id, payload) : await API.createShareUser(payload);
      closeModal();
      Toast.success(editing ? '分享账号已保存' : '分享账号已创建');
      await loadShareUsers();
      if (!editing && saved && saved.id) {
        const result = await API.getShareUserText(saved.id);
        if (result && result.text) shareShowText(result.text);
      }
    } catch (error) {
      Toast.error(error.message || '保存失败');
      button.disabled = false;
    }
  };
  openModal({ closeOnBackdrop: false });
}

async function openShareUserMore(user) {
  let detail = user;
  try { detail = await API.getShareUser(user.id); } catch (_) {}
  const sessions = (detail.sessions || []).map(session => `<li><strong>${esc(session.device_name || '未知设备')}</strong> · ${esc(session.client || '')} · ${esc(session.site_name || '')}<br><span class="form-help">${esc(session.client_ip || '')} · 最近活动 ${esc(session.last_seen_at || '')}</span></li>`).join('');
  document.getElementById('modal-title').textContent = `管理 ${user.username}`;
  document.getElementById('modal-body').innerHTML = `
    <div class="share-more-actions">
      <button type="button" class="btn-modal secondary" data-more="toggle">${user.enabled ? '停用账号' : '启用账号'}</button>
      <button type="button" class="btn-modal secondary" data-more="renew30">续期 30 天</button>
      <button type="button" class="btn-modal secondary" data-more="renew365">续期 1 年</button>
      <button type="button" class="btn-modal secondary" data-more="reset">重置已用流量</button>
      <button type="button" class="btn-modal secondary" data-more="kick">踢下所有设备</button>
      <button type="button" class="btn-modal secondary" data-more="portal">重置查询链接</button>
      <button type="button" class="btn-modal secondary share-danger" data-more="delete">删除账号</button>
    </div>
    <h4 class="share-more-heading">已登录设备（${(detail.sessions || []).length}）</h4>
    ${sessions ? `<ul class="share-session-list">${sessions}</ul>` : '<p class="form-help">暂无已登录设备。</p>'}`;
  document.getElementById('modal-footer').innerHTML = '<button class="btn-modal secondary" id="share-more-close">关闭</button>';
  document.getElementById('share-more-close').onclick = closeModal;
  document.querySelectorAll('[data-more]').forEach(button => {
    button.onclick = async () => {
      const action = button.dataset.more;
      try {
        if (action === 'toggle') {
          await API.updateShareUser(user.id, {
            username: user.username, password: '', note: user.note || '', enabled: !user.enabled,
            expires_at: user.expires_at || '', traffic_quota: Number(user.traffic_quota) || 0,
            max_devices: Number(user.max_devices) || 0, speed_limit: Number(user.speed_limit) || 0, traffic_reset_day: Number(user.traffic_reset_day) || 0,
            sites: (user.sites || []).map(site => ({ site_id: site.site_id, site_account_id: site.site_account_id || 0 })),
          });
          Toast.success(user.enabled ? '已停用，登录中的设备会被踢下线' : '已启用');
        } else if (action === 'renew30' || action === 'renew365') {
          await API.renewShareUser(user.id, action === 'renew30' ? 30 : 365);
          Toast.success('已续期');
        } else if (action === 'reset') {
          await API.resetShareUserTraffic(user.id);
          Toast.success('已用流量已清零');
        } else if (action === 'kick') {
          const result = await API.kickShareUser(user.id);
          Toast.success(`已踢下 ${result && result.revoked || 0} 个设备`);
        } else if (action === 'portal') {
          if (!window.confirm('重置后旧的查询链接会失效，需要把新链接重新发给朋友。继续？')) return;
          await API.resetShareUserPortal(user.id);
          Toast.success('查询链接已重置');
        } else if (action === 'delete') {
          if (!window.confirm(`确定删除分享账号 ${user.username}？登录中的设备会被踢下线，此操作无法撤销。`)) return;
          await API.deleteShareUser(user.id);
          Toast.success('分享账号已删除');
        }
        closeModal();
        await loadShareUsers();
      } catch (error) {
        Toast.error(error.message || '操作失败');
      }
    };
  });
  openModal({ closeOnBackdrop: true });
}

function renderShareUsers() {
  const page = document.getElementById('page-share-users');
  page.innerHTML = `
    <div class="share-users-page fade-up">
      <div class="page-toolbar">
        <span class="toolbar-info" id="share-user-summary">正在读取…</span>
        <button class="btn-primary share-create-btn" type="button" data-share-action="create">新建分享账号</button>
      </div>
      <p class="form-help share-intro">朋友在 Emby 客户端填入站点的反代地址，用这里的用户名和密码登录。服务器会用你保存的源站账号代为登录，视频全程经服务器中转。</p>
      <div id="share-user-container"></div>
    </div>`;
  if (!page.dataset.shareBound) {
    page.dataset.shareBound = '1';
    page.addEventListener('click', async event => {
      const button = event.target.closest('[data-share-action]');
      if (!button) return;
      const action = button.dataset.shareAction;
      const card = button.closest('[data-share-id]');
      const user = card ? shareUsersCache.find(item => String(item.id) === card.dataset.shareId) : null;
      if (action === 'create') return openShareUserForm(null);
      if (!user) return;
      if (action === 'edit') return openShareUserForm(user);
      if (action === 'more') return openShareUserMore(user);
      if (action === 'reveal') {
        const secret = card.querySelector('.share-secret');
        if (!secret) return;
        const hidden = secret.textContent === '••••••';
        secret.textContent = hidden ? secret.dataset.secret : '••••••';
        button.textContent = hidden ? '隐藏' : '显示';
        return;
      }
      if (action === 'portal') {
        const link = user.portal_url || (user.portal_path ? location.origin + user.portal_path : '');
        if (!link) return Toast.error('暂无查询链接');
        await shareCopyText(link, '查询链接已复制，朋友打开就能看到到期和流量');
        return;
      }
      if (action === 'copy') {
        try {
          const result = await API.getShareUserText(user.id);
          await shareCopyText(result.text, '分享信息已复制，可以直接发给朋友');
        } catch (error) {
          Toast.error(error.message || '生成分享信息失败');
        }
      }
    });
  }
  loadShareUsers();
}
