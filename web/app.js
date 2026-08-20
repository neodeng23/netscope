// netscope 体检台前端（无构建链，原生 JS）
// 所有检测都由按钮触发：单通道检测 / 全节点对比 / 趋势图均一次点击一次执行。
const $ = (s) => document.querySelector(s);
const state = { targets: [], nodes: [], groups: [], polling: null, trend: null };

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

function renderReports(reports) {
  const tb = $('#reports tbody');
  tb.innerHTML = '';
  $('#report-tip').hidden = reports.length > 0;
  for (const r of reports) {
    const tr = document.createElement('tr');
    const isHtml = r.name.endsWith('.html');
    tr.innerHTML = `
      <td class="wrap">${r.name}</td><td>${r.mtime}</td><td>${fmtSize(r.size)}</td>
      <td>${isHtml ? `<a href="/reports/${encodeURIComponent(r.name)}" target="_blank">打开</a>` : '-'}</td>`;
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

// ---------- 趋势图（SVG，一次点击一次绘制） ----------

function renderTrendNodeOptions() {
  const sel = $('#trend-node');
  const cur = sel.value;
  const names = new Set();
  for (const snap of state.trend || []) {
    for (const n of snap.nodes) names.add(n.node);
  }
  sel.innerHTML = '<option value="">选择节点</option>';
  [...names].sort().forEach((name) => {
    const o = document.createElement('option');
    o.value = name;
    o.textContent = name;
    sel.appendChild(o);
  });
  if (cur) sel.value = cur;
}

function drawTrend() {
  const node = $('#trend-node').value;
  const box = $('#trend-box');
  if (!node) { box.innerHTML = '<div class="tip">先选择要查看的节点</div>'; return; }
  const snaps = (state.trend || []).map((s) => ({
    time: s.label || s.time,
    n: s.nodes.find((x) => x.node === node),
  })).filter((s) => s.n);
  if (snaps.length < 2) {
    box.innerHTML = '<div class="tip">该节点的快照不足 2 次，多跑几次 <code>netscope sub rate</code> 再来</div>';
    return;
  }
  const W = 760, H = 220, L = 44, R = 16, T = 16, B = 42;
  const iw = W - L - R, ih = H - T - B;
  const x = (i) => L + (iw * i) / (snaps.length - 1);
  const y = (v) => T + ih * (1 - Math.max(0, Math.min(100, v)) / 100);
  const pts = snaps.map((s, i) => `${x(i).toFixed(1)},${y(s.n.total).toFixed(1)}`).join(' ');
  const first = snaps[0].n.total, last = snaps[snaps.length - 1].n.total;
  const delta = last - first;
  const lines = [];
  // 网格与刻度
  for (let v = 0; v <= 100; v += 25) {
    lines.push(`<line x1="${L}" y1="${y(v)}" x2="${W - R}" y2="${y(v)}" stroke="#e2e8f0" stroke-width="1"/>`);
    lines.push(`<text x="${L - 6}" y="${y(v) + 4}" text-anchor="end" font-size="10" fill="#94a3b8">${v}</text>`);
  }
  snaps.forEach((s, i) => {
    if (snaps.length <= 8 || i % Math.ceil(snaps.length / 8) === 0 || i === snaps.length - 1) {
      const label = s.time.length >= 10 ? s.time.slice(5, 10) : s.time;
      lines.push(`<text x="${x(i)}" y="${H - 18}" text-anchor="middle" font-size="10" fill="#94a3b8">${escapeHTML(label)}</text>`);
      lines.push(`<line x1="${x(i)}" y1="${T}" x2="${x(i)}" y2="${T + ih}" stroke="#f1f5f9" stroke-width="1"/>`);
    }
    lines.push(`<circle cx="${x(i)}" cy="${y(s.n.total)}" r="3" fill="#2563eb"/>`);
  });
  box.innerHTML = `
    <div class="trend-head">${escapeHTML(node)} · 最新 ${last.toFixed(0)} 分（${delta >= 0 ? '+' : ''}${delta.toFixed(0)}）· 共 ${snaps.length} 次快照</div>
    <svg viewBox="0 0 ${W} ${H}" class="trend-svg" role="img">
      ${lines.join('\n')}
      <polyline points="${pts}" fill="none" stroke="#2563eb" stroke-width="2" stroke-linejoin="round"/>
    </svg>`;
}

// ---------- 交互 ----------

async function refresh() {
  const s = await api('/api/state');
  state.targets = s.targets || [];
  state.groups = s.groups || [];
  state.nodes = s.nodes || [];
  renderGroupFilter();
  renderTargets();
  renderNodes();
  renderReports(s.reports || []);
}

async function refreshTrend() {
  state.trend = await api('/api/trend');
  renderTrendNodeOptions();
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

$('#btn-add').addEventListener('click', addTarget);
$('#new-url').addEventListener('keydown', (e) => { if (e.key === 'Enter') addTarget(); });
$('#btn-run').addEventListener('click', () => runChecks($('#via').value));
$('#btn-run-all').addEventListener('click', () => runChecks('all'));
$('#btn-refresh').addEventListener('click', async () => {
  await refresh();
  await refreshTrend();
});
$('#btn-export').addEventListener('click', () => { location.href = '/api/targets/export'; });
$('#file-import').addEventListener('change', (e) => {
  if (e.target.files.length > 0) importTargets(e.target.files[0]);
  e.target.value = '';
});
$('#btn-trend').addEventListener('click', drawTrend);
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
refreshTrend().catch(() => {});
