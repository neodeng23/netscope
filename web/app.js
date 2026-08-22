// netscope 体检台前端（无构建链，原生 JS）
// 所有检测都由按钮触发：单通道检测 / 全节点对比 / 趋势图均一次点击一次执行。
const $ = (s) => document.querySelector(s);
const state = { targets: [], subs: [], nodes: [], groups: [], polling: null, subPolling: null };

// ---------- API ----------

async function api(path, opts) {
  const r = await fetch(path, opts);
  if (!r.ok) throw new Error((await r.text()) || r.statusText);
  return r.json();
}

// ---------- 渲染 ----------

function renderTargets() {
  const filter = $('#group-filter').value;
  const tb = $('#targets tbody');
  tb.innerHTML = '';
  const list = state.targets.filter((t) => !filter || t.group === filter);
  $('#empty-tip').hidden = list.length > 0;
  for (const t of list) {
    const tr = document.createElement('tr');
    const group = `<span class="tag group-tag${t.group ? '' : ' muted'}" data-group="${t.id}" title="点击修改分组">${escapeHTML(t.group || '未分组')}</span>`;
    tr.innerHTML = `
      <td><input type="checkbox" class="pick" value="${t.id}" checked></td>
      <td class="wrap">${escapeHTML(t.url)}${t.note ? ' <span class="tag">' + escapeHTML(t.note) + '</span>' : ''}</td>
      <td>${group}</td>
      <td>${fmtTime(t.created)}</td>
      <td><button class="danger" data-del="${t.id}">删除</button></td>`;
    tb.appendChild(tr);
  }
}

function renderGroupFilter() {
  const sel = $('#group-filter');
  const cur = sel.value;
  sel.innerHTML = '<option value="">全部分组</option>';
  state.groups.forEach((g) => {
    const o = document.createElement('option');
    o.value = g;
    o.textContent = g;
    sel.appendChild(o);
  });
  if (cur) sel.value = cur;
}

function renderResult(item, withNode) {
  const tr = document.createElement('tr');
  const nodeCell = withNode ? `<td class="wrap">${escapeHTML(item.node || '-')}</td>` : '';
  if (!item.result) {
    tr.innerHTML = `<td class="wrap">${escapeHTML(item.url)}</td>${nodeCell}
      <td><span class="pill run">检测中…</span></td><td>-</td><td>-</td><td>-</td><td>-</td><td>-</td><td>-</td>`;
    return tr;
  }
  const r = item.result;
  const ok = r.ok;
  tr.innerHTML = `
    <td class="wrap">${escapeHTML(item.url)}</td>${nodeCell}
    <td>${ok ? '<span class="pill ok">✅ 可访问</span>' : '<span class="pill bad">❌ 不可达</span>'}</td>
    <td>${ok ? r.status : '-'}</td>
    <td>${r.totalMs ? r.totalMs.toFixed(1) + 'ms' : '-'}</td>
    <td>${r.connMs ? r.connMs.toFixed(1) + 'ms' : '-'}</td>
    <td>${escapeHTML(r.exitIp || '-')}${r.via && r.via !== 'direct' ? '<span class="tag">' + escapeHTML(r.via) + '</span>' : ''}</td>
    <td>${escapeHTML(r.location || '-')}${r.ipFlags ? '<span class="tag">' + escapeHTML(r.ipFlags) + '</span>' : ''}</td>
    <td class="wrap" style="color:var(--sub)">${escapeHTML(r.err || '')}</td>`;
  return tr;
}

function renderResults(job) {
  const withNode = job.items.some((it) => it.node);
  $('#th-node').hidden = !withNode;
  const tb = $('#results tbody');
  tb.innerHTML = '';
  $('#result-tip').hidden = true;
  for (const item of job.items) tb.appendChild(renderResult(item, withNode));
  const p = $('#progress');
  if (job.finished) {
    p.textContent = `完成：${job.done}/${job.total} · ${job.via}`;
    $('#btn-run').disabled = false;
    $('#btn-run-all').disabled = false;
    if (state.polling) { clearInterval(state.polling); state.polling = null; }
  } else {
    p.textContent = `检测中 ${job.done}/${job.total} · ${job.via}`;
  }
}


function renderSubs() {
  const tb = $('#subs tbody');
  tb.innerHTML = '';
  $('#subs-tip').hidden = state.subs.length > 0;
  const loading = state.subs.some((x) => !x.nodes && !x.err);
  $('#subs-progress').textContent = loading ? '加载中…' : '';
  for (const sub of state.subs) {
    const tr = document.createElement('tr');
    let count;
    if (sub.err) count = `<span class="pill bad" title="${escapeHTML(sub.err)}">失败</span>`;
    else if (sub.nodes > 0) count = `<span class="pill ok">${sub.nodes}</span>`;
    else count = '<span class="pill run">…</span>';
    tr.innerHTML = `
      <td class="sub-cell" title="${escapeHTML(sub.url)}">${escapeHTML(sub.url)}${sub.note ? '<span class="sub-note">' + escapeHTML(sub.note) + '</span>' : ''}</td>
      <td>${count}</td>
      <td>${fmtTime(sub.added)}</td>
      <td><button class="danger" data-sub-del="${sub.id}">删除</button></td>`;
    tb.appendChild(tr);
  }
}

function renderNodes() {
  const sel = $('#via');
  const cur = sel.value;
  sel.innerHTML = '<option value="direct">⏱ 直连（本机）</option>';
  state.nodes.forEach((n, i) => {
    const o = document.createElement('option');
    o.value = n.name;
    o.textContent = `${i + 1}. ${n.name}（${n.type}）`;
    sel.appendChild(o);
  });
  if (cur) sel.value = cur;
}

// ---------- 交互 ----------

async function refresh() {
  const s = await api('/api/state');
  state.targets = s.targets || [];
  state.subs = s.subs || [];
  state.groups = s.groups || [];
  state.nodes = s.nodes || [];
  renderSubs();
  renderGroupFilter();
  renderTargets();
  renderNodes();
}

async function addSub() {
  const input = $('#new-sub');
  const url = input.value.trim();
  if (!url) return;
  try {
    const { added } = await api('/api/subs', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ url }),
    });
    if (!added) { alert('该订阅已在清单中'); }
    input.value = '';
    await refresh();
    watchSubsLoading();
  } catch (e) { alert('添加失败：' + e.message); }
}

// 添加订阅后轮询,直到节点数出现或报错(最长 2 分钟)
function watchSubsLoading() {
  if (state.subPolling) clearInterval(state.subPolling);
  const deadline = Date.now() + 120 * 1000;
  state.subPolling = setInterval(async () => {
    try { await refresh(); } catch (e) { return; }
    const busy = state.subs.some((x) => !x.nodes && !x.err);
    if (!busy || Date.now() > deadline) {
      clearInterval(state.subPolling);
      state.subPolling = null;
    }
  }, 1200);
}

async function delSub(id) {
  await api('/api/subs/' + id, { method: 'DELETE' });
  await refresh();
  watchSubsLoading();
}

async function addTarget() {
  const input = $('#new-url');
  const url = input.value.trim();
  if (!url) return;
  try {
    await api('/api/targets', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ url, group: $('#new-group').value.trim() }),
    });
    input.value = '';
    await refresh();
  } catch (e) { alert('添加失败：' + e.message); }
}

async function editGroup(id) {
  const t = state.targets.find((x) => x.id === id);
  if (!t) return;
  const group = prompt('修改分组（留空为未分组）：', t.group || '');
  if (group === null) return;
  try {
    await api('/api/targets/update', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id, group: group.trim() }),
    });
    await refresh();
  } catch (e) { alert('修改失败：' + e.message); }
}

async function delTarget(id) {
  await api('/api/targets/' + id, { method: 'DELETE' });
  await refresh();
}

async function importTargets(file) {
  try {
    const items = JSON.parse(await file.text());
    if (!Array.isArray(items)) throw new Error('导入内容需为 JSON 数组');
    const { added } = await api('/api/targets/import', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(items),
    });
    alert(`导入完成：新增 ${added} 条（重复已跳过）`);
    await refresh();
  } catch (e) { alert('导入失败：' + e.message); }
}

async function runChecks(via) {
  const ids = [...document.querySelectorAll('.pick:checked')].map((c) => c.value);
  if (ids.length === 0) { alert('请先勾选要检测的地址'); return; }
  $('#btn-run').disabled = true;
  $('#btn-run-all').disabled = true;
  $('#result-tip').hidden = true;
  $('#results tbody').innerHTML = '';
  try {
    const { id } = await api('/api/jobs', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ids, via }),
    });
    state.polling = setInterval(async () => {
      const job = await api('/api/jobs/' + id);
      renderResults(job);
    }, 700);
  } catch (e) {
    alert('发起检测失败：' + e.message);
    $('#btn-run').disabled = false;
    $('#btn-run-all').disabled = false;
  }
}

// ---------- 本机网络体检 ----------

function siteRows(sites) {
  return (sites || []).map((x) => `
    <tr>
      <td>${escapeHTML(x.name)}</td>
      <td>${x.ok ? '<span class="pill ok">✅ ' + (x.status || '') + '</span>' : '<span class="pill bad">❌</span>'}</td>
      <td>${x.totalMs ? x.totalMs.toFixed(1) + 'ms' : '-'}</td>
    </tr>`).join('');
}

function pingLine(p) {
  if (!p || !p.recv) return '❌ 不通';
  return `avg ${p.avgMs.toFixed(1)}ms · 抖动 ${p.jitterMs.toFixed(1)}ms · 丢包 ${p.lossPct.toFixed(0)}%`;
}

function perspectivePanel(title, icon, p, extraRows) {
  const head = extraRows.map(([k, v]) => `<div class="kv"><span>${k}</span><b>${v}</b></div>`).join('');
  return `
  <div class="local-col">
    <h3>${icon} ${title}</h3>
    ${p.err && !p.ip ? `<div class="kv"><span>查询失败</span><b>${escapeHTML(p.err)}</b></div>` : ''}
    <div class="kv"><span>出口 IP</span><b>${escapeHTML(p.ip || '-')}</b></div>
    <div class="kv"><span>归属地</span><b>${escapeHTML(p.location || '-')}</b></div>
    ${head}
    <div class="kv"><span>基准延迟</span><b>${pingLine(p.ping)}</b></div>
    <table class="grid"><thead><tr><th>站点</th><th>状态</th><th>耗时</th></tr></thead><tbody>${siteRows(p.sites)}</tbody></table>
  </div>`;
}

function renderLocal(r) {
  const box = $('#local-box');
  const fExtra = [];
  if (r.foreign) {
    if (r.foreign.isp) fExtra.push(['ISP', escapeHTML(r.foreign.isp)]);
    if (r.foreign.flags) fExtra.push(['IP 标记', escapeHTML(r.foreign.flags)]);
    if (r.foreign.risk >= 0) fExtra.push(['风险分', r.foreign.risk + '/100']);
  }
  let dnsLine = '未检测';
  if (r.dns && r.dns.detail) {
    const tag = r.dns.polluted === true ? '<span class="pill bad">疑似污染</span>'
      : r.dns.polluted === false ? '<span class="pill ok">正常</span>' : '<span class="pill run">无法判定</span>';
    dnsLine = `${tag} ${escapeHTML(r.dns.detail)}`;
  }
  let exitLine = '未知';
  if (r.sameExit !== null && r.sameExit !== undefined) {
    exitLine = r.sameExit
      ? '<span class="pill ok">一致</span> 国内外看到的出口相同（无分流）'
      : '<span class="pill bad">不一致</span> 国内外看到的出口不同（存在代理/分流/TUN）';
  }
  box.innerHTML = `
    <div class="kv-line">体检时间 ${new Date(r.time).toLocaleString()}</div>
    <div class="local-grid">
      ${perspectivePanel('从国内测试', '🇨🇳', r.domestic || {}, [])}
      ${perspectivePanel('从国外测试', '🌐', r.foreign || {}, fExtra)}
    </div>
    <div class="kv-line">DNS：${dnsLine}</div>
    <div class="kv-line">出口一致性：${exitLine}</div>`;
}

async function runLocal() {
  const btn = $('#btn-local');
  btn.disabled = true;
  $('#local-progress').textContent = '体检中…（约 5~15 秒）';
  try {
    const r = await fetch('/api/local', { method: 'POST' });
    if (!r.ok) throw new Error(await r.text());
    renderLocal(await r.json());
    $('#local-progress').textContent = '完成';
  } catch (e) {
    $('#local-progress').textContent = '';
    alert('体检失败：' + e.message);
  } finally {
    btn.disabled = false;
  }
}

// ---------- 工具 ----------

function escapeHTML(s) {
  return String(s ?? '').replace(/[&<>"']/g, (c) => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
  }[c]));
}
function fmtTime(iso) {
  if (!iso) return '-';
  const d = new Date(iso);
  return isNaN(d) ? '-' : `${d.getMonth() + 1}-${d.getDate()} ${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;
}
function fmtSize(n) {
  if (!n && n !== 0) return '-';
  if (n > 1 << 20) return (n / (1 << 20)).toFixed(1) + ' MB';
  if (n > 1024) return (n / 1024).toFixed(0) + ' KB';
  return n + ' B';
}

// ---------- 启动 ----------

$('#btn-sub-add').addEventListener('click', addSub);
$('#new-sub').addEventListener('keydown', (e) => { if (e.key === 'Enter') addSub(); });
$('#subs tbody').addEventListener('click', (e) => {
  const id = e.target.dataset && e.target.dataset.subDel;
  if (id) delSub(id);
});
$('#btn-add').addEventListener('click', addTarget);
$('#new-url').addEventListener('keydown', (e) => { if (e.key === 'Enter') addTarget(); });
$('#btn-run').addEventListener('click', () => runChecks($('#via').value));
$('#btn-run-all').addEventListener('click', () => runChecks('all'));
$('#btn-refresh').addEventListener('click', async () => {
  await refresh();
});
$('#btn-export').addEventListener('click', () => { location.href = '/api/targets/export'; });
$('#file-import').addEventListener('change', (e) => {
  if (e.target.files.length > 0) importTargets(e.target.files[0]);
  e.target.value = '';
});
$('#check-all').addEventListener('change', (e) => {
  document.querySelectorAll('.pick').forEach((c) => (c.checked = e.target.checked));
});
$('#group-filter').addEventListener('change', renderTargets);
$('#targets tbody').addEventListener('click', (e) => {
  const t = e.target;
  if (t.dataset && t.dataset.del) { delTarget(t.dataset.del); return; }
  if (t.dataset && t.dataset.group) { editGroup(t.dataset.group); }
});
refresh().catch((e) => {
  $('#result-tip').textContent = '加载失败：' + e.message + '（如配置了 --token，需在 URL 加 ?token=xxx）';
});
$('#btn-local').addEventListener('click', runLocal);
